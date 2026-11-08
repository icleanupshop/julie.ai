package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	t.Setenv("JULIE_API_KEY", "override")
	t.Setenv("JULIE_EMBEDDING_API_KEY", "embedding-override")
	path := filepath.Join(t.TempDir(), "model.json")
	fixture := `{"apiKey":"test-key","apiType":"chat-completions","models":[{"id":"test-model","url":"https://example.com/v1/chat/completions","toolCalling":true}],"embeddings":{"url":"https://example.com/v1/embeddings","model":"test-embedding"},"vectorStore":{"url":"http://qdrant:6333","collection":"docs"}}`
	if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	config, model, err := LoadConfig(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if config.APIKey != "override" || config.EmbeddingAPIKey != "embedding-override" || config.Embeddings.Model != "test-embedding" || config.VectorStore.Collection != "docs" || model.ID != "test-model" || !model.ToolCalling {
		t.Fatalf("unexpected config or model")
	}
	if _, _, err := LoadConfig(path, "missing"); err == nil {
		t.Fatal("expected missing model error")
	}
	if err := os.WriteFile(path, []byte(`{"models":[{"id":"bad","url":"file:///etc/passwd","apiType":"chat-completions"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadConfig(path, ""); err == nil {
		t.Fatal("expected invalid endpoint error")
	}
}

func TestComplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" || request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("incorrect endpoint, method, or authorization")
		}
		var payload completionRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload.Model != "test-model" || len(payload.Messages) != 1 || payload.Stream || len(payload.Tools) != 1 || payload.ToolChoice != "auto" {
			t.Errorf("unexpected request: %+v", payload)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello"}}]}`))
	}))
	defer server.Close()
	client := NewClient(Config{APIKey: "test-key"}, Model{ID: "test-model", URL: server.URL + "/v1/chat/completions", ToolCalling: true})
	message, err := client.Complete(context.Background(), []Message{{Role: "user", Content: "Hi"}}, []Tool{{Type: "function"}})
	if err != nil || message.Content != "Hello" {
		t.Fatalf("message=%+v err=%v", message, err)
	}
}

func TestCompleteErrors(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", 401, `secret response`},
		{"malformed", 200, `{`},
		{"no choices", 200, `{"choices":[]}`},
		{"empty message", 200, `{"choices":[{"message":{"role":"assistant","content":null}}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(test.status)
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()
			client := NewClient(Config{}, Model{ID: "test", URL: server.URL})
			if _, err := client.Complete(context.Background(), nil, nil); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
