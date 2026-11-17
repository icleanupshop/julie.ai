package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"julie.ai/internal/knowledge"
)

type testState struct {
	sessions map[int64]string
	lastID   int64
}

func (state *testState) SessionForUser(_ context.Context, userID int64) (string, bool, error) {
	sessionID, ok := state.sessions[userID]
	return sessionID, ok, nil
}

func (state *testState) SaveSession(_ context.Context, userID int64, sessionID string) error {
	state.sessions[userID] = sessionID
	return nil
}

func (state *testState) LastUpdateID(context.Context) (int64, error) { return state.lastID, nil }

func (state *testState) SetLastUpdateID(_ context.Context, updateID int64) error {
	state.lastID = updateID
	return nil
}

func (state *testState) Close() error { return nil }

type rememberedMessage struct {
	chatID    int64
	userID    int64
	messageID int64
	direction string
	text      string
}

type testHistory struct {
	messages []rememberedMessage
}

func (history *testHistory) RememberTelegramMessage(_ context.Context, chatID, userID, messageID int64, direction, text string) error {
	history.messages = append(history.messages, rememberedMessage{chatID: chatID, userID: userID, messageID: messageID, direction: direction, text: text})
	return nil
}

func (history *testHistory) SearchTelegramHistory(context.Context, int64, string, int) ([]knowledge.Result, error) {
	return nil, nil
}

func TestIncomingMessageAllowlistDefaultsToDenyAndCombinesRules(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		chatID  int64
		userID  int64
		allowed bool
	}{
		{name: "empty default deny", allowed: false},
		{name: "chat-only allowlist denies", config: Config{AllowedChatIDs: []int64{42}}, chatID: 42, userID: 42, allowed: false},
		{name: "user allow", config: Config{AllowedUserIDs: []int64{7}}, chatID: 42, userID: 7, allowed: true},
		{name: "chat restriction allow", config: Config{AllowedChatIDs: []int64{42}, AllowedUserIDs: []int64{7}}, chatID: 42, userID: 7, allowed: true},
		{name: "chat restriction deny", config: Config{AllowedChatIDs: []int64{42}, AllowedUserIDs: []int64{7}}, chatID: 43, userID: 7, allowed: false},
		{name: "user deny", config: Config{AllowedUserIDs: []int64{7}}, chatID: 8, userID: 8, allowed: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, err := NewService(test.config, "test-token", &testHistory{}, &testState{sessions: make(map[int64]string)})
			if err != nil {
				t.Fatal(err)
			}
			if allowed := service.allowedIncoming(test.chatID, test.userID); allowed != test.allowed {
				t.Fatalf("allowedIncoming(%d, %d)=%t, want %t", test.chatID, test.userID, allowed, test.allowed)
			}
		})
	}
}

func TestTelegramAPIRequestErrorsDoNotExposeBotToken(t *testing.T) {
	service, err := NewService(Config{}, "secret-bot-token", &testHistory{}, &testState{sessions: make(map[int64]string)})
	if err != nil {
		t.Fatal(err)
	}
	service.apiOrigin = "http://127.0.0.1:1"
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, service.apiOrigin+"/bot"+service.token+"/getUpdates", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.call(request, &struct{}{}); err == nil || strings.Contains(err.Error(), service.token) {
		t.Fatalf("expected sanitized transport error, got %v", err)
	}
}

func TestAllowedPrivateMessageGetsJulieAnswerAndIsStored(t *testing.T) {
	state := &testState{sessions: make(map[int64]string)}
	history := &testHistory{}
	var createdSessions, agentTurns, sentMessages int
	julieServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/sessions":
			createdSessions++
			if request.Method != http.MethodPost {
				t.Errorf("session method=%s, want POST", request.Method)
			}
			_, _ = writer.Write([]byte(`{"session_id":"session-7"}`))
		case "/sessions/session-7/messages":
			agentTurns++
			var payload map[string]string
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil || payload["prompt"] != "hello Julie" {
				t.Errorf("unexpected Julie turn: %+v err=%v", payload, err)
			}
			_, _ = writer.Write([]byte(`{"session_id":"session-7","answer":"hello back"}`))
		default:
			t.Errorf("unexpected Julie URL: %s", request.URL.Path)
			http.NotFound(writer, request)
		}
	}))
	defer julieServer.Close()
	telegramServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/bottest-token/sendMessage" || request.Method != http.MethodPost {
			t.Errorf("unexpected Telegram request: %s %s", request.Method, request.URL.Path)
			http.NotFound(writer, request)
			return
		}
		var payload struct {
			ChatID int64  `json:"chat_id"`
			Text   string `json:"text"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil || payload.ChatID != 7 || payload.Text != "hello back" {
			t.Errorf("unexpected Telegram reply: %+v err=%v", payload, err)
		}
		sentMessages++
		_, _ = fmt.Fprintf(writer, `{"ok":true,"result":{"message_id":%d}}`, 100+sentMessages)
	}))
	defer telegramServer.Close()
	service, err := NewService(Config{AllowedUserIDs: []int64{7}, JulieURL: julieServer.URL}, "test-token", history, state)
	if err != nil {
		t.Fatal(err)
	}
	service.apiOrigin = telegramServer.URL
	item := update{ID: 11, Message: &struct {
		ID   int64 `json:"message_id"`
		Chat struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
		From *struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Text string `json:"text"`
	}{ID: 3, Text: "hello Julie"}}
	item.Message.Chat.ID = 7
	item.Message.Chat.Type = "private"
	item.Message.From = &struct {
		ID int64 `json:"id"`
	}{ID: 7}
	if err := service.processUpdate(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if createdSessions != 1 || agentTurns != 1 || sentMessages != 1 {
		t.Fatalf("sessions=%d agent turns=%d sent messages=%d", createdSessions, agentTurns, sentMessages)
	}
	if state.sessions[7] != "session-7" {
		t.Fatalf("saved session=%q", state.sessions[7])
	}
	if len(history.messages) != 2 || history.messages[0].direction != "incoming" || history.messages[1].direction != "outgoing" || history.messages[1].text != "hello back" {
		t.Fatalf("unexpected remembered history: %+v", history.messages)
	}
}

func TestProcessUpdateIgnoresDisallowedAndGroupMessages(t *testing.T) {
	state := &testState{sessions: make(map[int64]string)}
	history := &testHistory{}
	julieServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Errorf("Julie should not be called for a rejected message")
		http.Error(writer, "unexpected", http.StatusInternalServerError)
	}))
	defer julieServer.Close()
	service, err := NewService(Config{AllowedUserIDs: []int64{7}, JulieURL: julieServer.URL}, "test-token", history, state)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		chatID int64
		userID int64
		typeID string
	}{
		{name: "unlisted user", chatID: 8, userID: 8, typeID: "private"},
		{name: "group message from allowed user", chatID: -100, userID: 7, typeID: "supergroup"},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := privateMessageUpdate(test.chatID, test.userID, test.typeID, "do not process")
			if err := service.processUpdate(context.Background(), item); err != nil {
				t.Fatal(err)
			}
		})
	}
	if len(history.messages) != 0 {
		t.Fatalf("rejected messages were stored: %+v", history.messages)
	}
}

func privateMessageUpdate(chatID, userID int64, chatType, text string) update {
	message := &struct {
		ID   int64 `json:"message_id"`
		Chat struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
		From *struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Text string `json:"text"`
	}{ID: 3, Text: text}
	message.Chat.ID = chatID
	message.Chat.Type = chatType
	message.From = &struct {
		ID int64 `json:"id"`
	}{ID: userID}
	return update{ID: 11, Message: message}
}
