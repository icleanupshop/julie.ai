package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"julie.ai/internal/sessions"
)

func TestHTTPHandler(t *testing.T) {
	store := openTestSessions(t)
	calls := 0
	handler := newHTTPHandler(time.Second, store, func(ctx context.Context, sessionID string, history []sessions.Message, prompt string) (string, error) {
		calls++
		if prompt != "Hello" || sessionID != "" || len(history) != 0 {
			t.Errorf("unexpected prompt %q", prompt)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("missing request deadline")
		}
		return "Hi", nil
	}, nil, nil)
	for _, test := range []struct {
		name          string
		method        string
		path          string
		authorization string
		body          string
		status        int
	}{
		{"health", "GET", "/healthz", "", "", 200},
		{"ask without token", "POST", "/ask", "", `{"prompt":"Hello"}`, 200},
		{"ask with ignored authorization header", "POST", "/ask", "Bearer ignored", `{"prompt":"Hello"}`, 200},
		{"wrong method", "GET", "/ask", "", "", 405},
		{"bad JSON", "POST", "/ask", "", `{`, 400},
		{"empty prompt", "POST", "/ask", "", `{"prompt":" "}`, 400},
		{"unknown field", "POST", "/ask", "", `{"prompt":"Hello","extra":true}`, 400},
		{"trailing JSON", "POST", "/ask", "", `{"prompt":"Hello"}{}`, 400},
		{"large request", "POST", "/ask", "", `{"prompt":"` + strings.Repeat("x", 1<<20) + `"}`, 413},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			if test.authorization != "" {
				request.Header.Set("Authorization", test.authorization)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if strings.HasPrefix(test.name, "ask ") && response.Body.String() != "{\"answer\":\"Hi\"}\n" {
				t.Fatalf("unexpected response %s", response.Body.String())
			}
		})
	}
	if calls != 2 {
		t.Fatalf("expected two model calls, got %d", calls)
	}
}

func TestHTTPSessionLifecycle(t *testing.T) {
	store := openTestSessions(t)
	var forgotten string
	remembered := 0
	turnsBySession := make(map[string]int)
	handler := newHTTPHandler(30*time.Second, store, func(_ context.Context, sessionID string, history []sessions.Message, prompt string) (string, error) {
		if sessionID == "" {
			t.Error("session ID was not passed to agent")
		}
		if len(history) != turnsBySession[sessionID]*2 {
			t.Errorf("session %q history has %d messages before turn %d", sessionID, len(history), turnsBySession[sessionID]+1)
		}
		return "answer: " + prompt, nil
	}, func(_ context.Context, sessionID string, turnID int64, prompt, answer string) error {
		if sessionID == "" || turnID < 1 || prompt == "" || answer == "" {
			t.Errorf("invalid memory callback: session=%q turn=%d prompt=%q answer=%q", sessionID, turnID, prompt, answer)
		}
		remembered++
		turnsBySession[sessionID]++
		return nil
	}, func(_ context.Context, sessionID string) error {
		forgotten = sessionID
		return nil
	})

	createSession := func() sessions.Session {
		create := httptest.NewRecorder()
		handler.ServeHTTP(create, httptest.NewRequest("POST", "/sessions", nil))
		if create.Code != 201 {
			t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
		}
		var created sessions.Session
		if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil || created.ID == "" {
			t.Fatalf("created session=%+v err=%v", created, err)
		}
		return created
	}
	created := createSession()
	other := createSession()

	sendMessage := func(sessionID, prompt string) {
		request := httptest.NewRequest("POST", "/sessions/"+sessionID+"/messages", strings.NewReader(`{"prompt":"`+prompt+`"}`))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("message status=%d body=%s", response.Code, response.Body.String())
		}
	}
	sendMessage(created.ID, "first")
	sendMessage(other.ID, "separate")
	sendMessage(created.ID, "second")
	if remembered != 3 {
		t.Fatalf("memory writes=%d, want 3", remembered)
	}

	read := httptest.NewRecorder()
	handler.ServeHTTP(read, httptest.NewRequest("GET", "/sessions/"+created.ID, nil))
	var transcript struct {
		SessionID string             `json:"session_id"`
		Messages  []sessions.Message `json:"messages"`
	}
	if read.Code != 200 || json.Unmarshal(read.Body.Bytes(), &transcript) != nil || transcript.SessionID != created.ID || len(transcript.Messages) != 4 {
		t.Fatalf("read status=%d transcript=%+v body=%s", read.Code, transcript, read.Body.String())
	}
	if transcript.Messages[0].Content != "first" || transcript.Messages[2].Content != "second" {
		t.Fatalf("unexpected first-session transcript: %+v", transcript.Messages)
	}
	otherRead := httptest.NewRecorder()
	handler.ServeHTTP(otherRead, httptest.NewRequest("GET", "/sessions/"+other.ID, nil))
	var otherTranscript struct {
		Messages []sessions.Message `json:"messages"`
	}
	if otherRead.Code != 200 || json.Unmarshal(otherRead.Body.Bytes(), &otherTranscript) != nil || len(otherTranscript.Messages) != 2 || otherTranscript.Messages[0].Content != "separate" {
		t.Fatalf("other session transcript leaked or missing: status=%d transcript=%+v", otherRead.Code, otherTranscript)
	}

	remove := httptest.NewRecorder()
	handler.ServeHTTP(remove, httptest.NewRequest("DELETE", "/sessions/"+created.ID, nil))
	if remove.Code != 204 || forgotten != created.ID {
		t.Fatalf("delete status=%d forgotten=%q", remove.Code, forgotten)
	}
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest("GET", "/sessions/"+created.ID, nil))
	if missing.Code != 404 {
		t.Fatalf("deleted session status=%d", missing.Code)
	}
}

func TestHTTPHandlerErrors(t *testing.T) {
	store := openTestSessions(t)
	for _, test := range []struct {
		name   string
		err    error
		status int
	}{
		{"upstream", errors.New("private upstream details"), 502},
		{"timeout", context.DeadlineExceeded, 504},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := newHTTPHandler(time.Second, store, func(context.Context, string, []sessions.Message, string) (string, error) {
				return "", test.err
			}, nil, nil)
			request := httptest.NewRequest("POST", "/ask", strings.NewReader(`{"prompt":"Hello"}`))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status || strings.Contains(response.Body.String(), test.err.Error()) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func openTestSessions(t *testing.T) *sessions.Store {
	t.Helper()
	store, err := sessions.Open(context.Background(), filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
