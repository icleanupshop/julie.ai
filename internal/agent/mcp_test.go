package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPToolsRoundTripAndAuthentication(t *testing.T) {
	root := testWorkspace(t)
	defer root.Close()
	searcher := &testKnowledge{}
	handler, err := NewMCPHTTPHandler(NewMCPServer(root, searcher), "test-token")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	response, err := http.Get(server.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d, want %d", response.StatusCode, http.StatusUnauthorized)
	}

	remote, err := NewMCPToolClient(context.Background(), server.URL+"/mcp", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	tools, err := remote.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 3 {
		t.Fatalf("discovered %d tools, want 3: %+v", len(tools), tools)
	}

	file, err := remote.Call(context.Background(), FunctionCall{Name: "read_file", Arguments: `{"path":"hello.txt"}`})
	if err != nil || file != "hello workspace" {
		t.Fatalf("read_file=%q err=%v", file, err)
	}
	listing, err := remote.Call(context.Background(), FunctionCall{Name: "list_files", Arguments: `{"path":"."}`})
	if err != nil || !strings.Contains(listing, "hello.txt") {
		t.Fatalf("list_files=%q err=%v", listing, err)
	}
	knowledgeResult, err := remote.Call(context.Background(), FunctionCall{Name: "search_knowledge", Arguments: `{"query":"token lifetime","limit":2}`})
	if err != nil || !strings.Contains(knowledgeResult, "short-lived tokens") {
		t.Fatalf("search_knowledge=%q err=%v", knowledgeResult, err)
	}
	if searcher.query != "token lifetime" || searcher.limit != 2 {
		t.Fatalf("query=%q limit=%d", searcher.query, searcher.limit)
	}
	if _, err := remote.Call(context.Background(), FunctionCall{Name: "shell", Arguments: `{}`}); err == nil {
		t.Fatal("unexpectedly allowed a tool outside the allowlist")
	}
}
