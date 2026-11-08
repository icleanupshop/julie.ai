package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPHandler(t *testing.T) {
	calls := 0
	handler := newHTTPHandler("test-token", time.Second, func(ctx context.Context, prompt string) (string, error) {
		calls++
		if prompt != "Hello" {
			t.Errorf("unexpected prompt %q", prompt)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("missing request deadline")
		}
		return "Hi", nil
	})
	for _, test := range []struct {
		name   string
		method string
		path   string
		token  string
		body   string
		status int
	}{
		{"health", "GET", "/healthz", "", "", 200},
		{"ask", "POST", "/ask", "test-token", `{"prompt":"Hello"}`, 200},
		{"unauthenticated", "POST", "/ask", "", `{"prompt":"Hello"}`, 401},
		{"wrong token", "POST", "/ask", "wrong", `{"prompt":"Hello"}`, 401},
		{"wrong method", "GET", "/ask", "test-token", "", 405},
		{"bad JSON", "POST", "/ask", "test-token", `{`, 400},
		{"empty prompt", "POST", "/ask", "test-token", `{"prompt":" "}`, 400},
		{"unknown field", "POST", "/ask", "test-token", `{"prompt":"Hello","extra":true}`, 400},
		{"trailing JSON", "POST", "/ask", "test-token", `{"prompt":"Hello"}{}`, 400},
		{"large request", "POST", "/ask", "test-token", `{"prompt":"` + strings.Repeat("x", 1<<20) + `"}`, 413},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			if test.token != "" {
				request.Header.Set("Authorization", "Bearer "+test.token)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if test.name == "ask" && response.Body.String() != "{\"answer\":\"Hi\"}\n" {
				t.Fatalf("unexpected response %s", response.Body.String())
			}
		})
	}
	if calls != 1 {
		t.Fatalf("expected one model call, got %d", calls)
	}
}

func TestHTTPHandlerErrors(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
	}{
		{"upstream", errors.New("private upstream details"), 502},
		{"timeout", context.DeadlineExceeded, 504},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := newHTTPHandler("token", time.Second, func(context.Context, string) (string, error) {
				return "", test.err
			})
			request := httptest.NewRequest("POST", "/ask", strings.NewReader(`{"prompt":"Hello"}`))
			request.Header.Set("Authorization", "Bearer token")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status || strings.Contains(response.Body.String(), test.err.Error()) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
