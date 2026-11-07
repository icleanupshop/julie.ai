package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentToolLoopAndHistory(t *testing.T) {
	root := testWorkspace(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		var payload completionRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		switch requests {
		case 1:
			if len(payload.Tools) != 2 || len(payload.Messages) != 2 {
				t.Error("expected tools and initial conversation")
			}
			_, _ = writer.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"hello.txt\"}"}}]}}]}`))
		case 2:
			if len(payload.Messages) != 4 {
				t.Errorf("expected 4 messages, got %d", len(payload.Messages))
			} else if last := payload.Messages[3]; last.Role != "tool" || last.ToolCallID != "call-1" || last.Content != "hello workspace" {
				t.Errorf("incorrect tool result: %+v", last)
			}
			_, _ = writer.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"The file says hello workspace."}}]}`))
		case 3:
			if len(payload.Messages) != 6 {
				t.Error("conversation history was not retained")
			}
			_, _ = writer.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Yes."}}]}`))
		default:
			t.Error("unexpected request")
		}
	}))
	defer server.Close()
	agent := New(NewClient(Config{}, Model{ID: "test", URL: server.URL, ToolCalling: true}), root, 4)
	answer, err := agent.Ask(context.Background(), "Read hello.txt")
	if err != nil || answer != "The file says hello workspace." {
		t.Fatalf("answer=%q err=%v", answer, err)
	}
	if _, err := agent.Ask(context.Background(), "Is that the content?"); err != nil {
		t.Fatal(err)
	}
	agent.Reset()
	if len(agent.history) != 1 {
		t.Fatal("reset did not clear conversation")
	}
}

func TestAgentStepLimitAndDisabledTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call","type":"function","function":{"name":"list_files","arguments":"{\"path\":\".\"}"}}]}}]}`))
	}))
	defer server.Close()
	client := NewClient(Config{}, Model{ID: "test", URL: server.URL, ToolCalling: true})
	for _, test := range []struct {
		name string
		root *os.Root
		want string
	}{
		{"limit", testWorkspace(t), "limit"},
		{"disabled", nil, "disabled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent := New(client, test.root, 2)
			if _, err := agent.Ask(context.Background(), "List files"); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %s error, got %v", test.want, err)
			}
			if len(agent.history) != 1 {
				t.Fatal("failed turn polluted conversation history")
			}
		})
	}
}

func TestWorkspaceTools(t *testing.T) {
	root := testWorkspace(t)
	agent := New(nil, root, 2)
	if result, err := agent.execute(FunctionCall{Name: "read_file", Arguments: `{"path":"hello.txt"}`}); err != nil || result != "hello workspace" {
		t.Fatalf("result=%q err=%v", result, err)
	}
	if result, err := agent.execute(FunctionCall{Name: "list_files", Arguments: `{"path":"."}`}); err != nil || !strings.Contains(result, "hello.txt") {
		t.Fatalf("result=%q err=%v", result, err)
	}
	for _, call := range []FunctionCall{
		{Name: "read_file", Arguments: `{"path":"../outside.txt"}`},
		{Name: "read_file", Arguments: `{"path":"escape.txt"}`},
		{Name: "read_file", Arguments: `{"path":"/etc/passwd"}`},
		{Name: "read_file", Arguments: `{"path":"large.txt"}`},
		{Name: "read_file", Arguments: `{"path":"."}`},
		{Name: "list_files", Arguments: `{"path":"hello.txt"}`},
		{Name: "read_file", Arguments: `{`},
		{Name: "read_file", Arguments: `{}`},
		{Name: "shell", Arguments: `{"path":"."}`},
	} {
		if _, err := agent.execute(call); err == nil {
			t.Errorf("expected rejection for %+v", call)
		}
	}
}

func testWorkspace(t *testing.T) *os.Root {
	t.Helper()
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"workspace/hello.txt": "hello workspace",
		"workspace/large.txt": strings.Repeat("x", (64<<10)+1),
		"outside.txt":         "private",
	} {
		if err := os.WriteFile(filepath.Join(parent, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../outside.txt", filepath.Join(workspace, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}
