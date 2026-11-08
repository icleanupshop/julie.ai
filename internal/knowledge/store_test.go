package knowledge

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

func TestIngestAndSearch(t *testing.T) {
	var created, deleted, upserted int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/collections/docs":
			if created == 0 {
				http.NotFound(writer, request)
				return
			}
			_, _ = writer.Write([]byte(`{"result":{"config":{"params":{"vectors":{"size":2,"distance":"Cosine"}}}}}`))
		case request.URL.Path == "/v1/embeddings":
			var payload struct {
				Model string `json:"model"`
				Input string `json:"input"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload.Model != "embed-test" || payload.Input == "" {
				t.Errorf("unexpected embedding request: %+v", payload)
			}
			_, _ = writer.Write([]byte(`{"data":[{"embedding":[0.25,0.75]}]}`))
		case request.Method == http.MethodPut && request.URL.Path == "/collections/docs":
			created++
			var payload struct {
				Vectors struct {
					Size     int    `json:"size"`
					Distance string `json:"distance"`
				} `json:"vectors"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil || payload.Vectors.Size != 2 || payload.Vectors.Distance != "Cosine" {
				t.Errorf("unexpected collection request: %+v err=%v", payload, err)
			}
			_, _ = writer.Write([]byte(`{"result":true,"status":"ok"}`))
		case request.Method == http.MethodPost && request.URL.Path == "/collections/docs/points/delete":
			deleted++
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil || !strings.Contains(request.URL.RawQuery, "wait=true") {
				t.Errorf("unexpected deletion request: %+v err=%v", payload, err)
			}
			_, _ = writer.Write([]byte(`{"result":{"operation_id":1},"status":"ok"}`))
		case request.Method == http.MethodPut && request.URL.Path == "/collections/docs/points":
			var payload struct {
				Points []point `json:"points"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			for _, item := range payload.Points {
				if item.Payload["source"] != "guide.md" || item.Payload["content_hash"] == nil || len(item.Vector) != 2 {
					t.Errorf("unexpected point: %+v", item)
				}
			}
			upserted += len(payload.Points)
			_, _ = writer.Write([]byte(`{"result":{"operation_id":2},"status":"ok"}`))
		case request.Method == http.MethodPost && request.URL.Path == "/collections/docs/points/search":
			var payload struct {
				Limit       int       `json:"limit"`
				Vector      []float64 `json:"vector"`
				WithPayload bool      `json:"with_payload"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil || payload.Limit != 2 || len(payload.Vector) != 2 || !payload.WithPayload {
				t.Errorf("unexpected search request: %+v err=%v", payload, err)
			}
			_, _ = writer.Write([]byte(`{"result":[{"score":0.91,"payload":{"source":"guide.md","title":"guide.md","text":"Use short-lived tokens."}}]}`))
		default:
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.String())
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	store, err := NewStore(EmbeddingConfig{URL: server.URL + "/v1/embeddings", Model: "embed-test"}, VectorStoreConfig{URL: server.URL, Collection: "docs"}, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "guide.md")
	if err := os.WriteFile(path, []byte(strings.Repeat("short-lived tokens are safer. ", 160)), 0600); err != nil {
		t.Fatal(err)
	}
	count, err := store.IngestPath(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if count < 2 || created != 1 || deleted != 1 || upserted != count {
		t.Fatalf("count=%d created=%d deleted=%d upserted=%d", count, created, deleted, upserted)
	}
	results, err := store.Search(context.Background(), "token lifetime", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Source != "guide.md" || results[0].Text != "Use short-lived tokens." || results[0].Score != 0.91 {
		t.Fatalf("unexpected results: %+v", results)
	}
	secondStore, err := NewStore(EmbeddingConfig{URL: server.URL + "/v1/embeddings", Model: "embed-test"}, VectorStoreConfig{URL: server.URL, Collection: "docs"}, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secondStore.Search(context.Background(), "token lifetime", 2); err != nil {
		t.Fatalf("reuse existing collection: %v", err)
	}
	if created != 1 {
		t.Fatalf("existing collection was recreated %d times", created-1)
	}
}

func TestNewStoreRequiresCompleteConfiguration(t *testing.T) {
	if store, err := NewStore(EmbeddingConfig{}, VectorStoreConfig{}, ""); err != nil || store != nil {
		t.Fatalf("store=%v err=%v", store, err)
	}
	for _, test := range []struct {
		name      string
		embedding EmbeddingConfig
		vectors   VectorStoreConfig
	}{
		{name: "incomplete", embedding: EmbeddingConfig{URL: "http://localhost/v1/embeddings"}},
		{name: "invalid embedding URL", embedding: EmbeddingConfig{URL: "file:///tmp/embed", Model: "test"}, vectors: VectorStoreConfig{URL: "http://localhost:6333"}},
		{name: "invalid collection", embedding: EmbeddingConfig{URL: "http://localhost/v1/embeddings", Model: "test"}, vectors: VectorStoreConfig{URL: "http://localhost:6333", Collection: "bad/name"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewStore(test.embedding, test.vectors, ""); err == nil {
				t.Fatal("expected invalid configuration error")
			}
		})
	}
}

func TestSplitChunksOverlapAndEmpty(t *testing.T) {
	text := strings.Repeat("x", chunkRunes*2)
	chunks := splitChunks(text)
	if len(chunks) != 3 || len(chunks[1]) != chunkRunes || chunks[0][len(chunks[0])-chunkOverlap:] != chunks[1][:chunkOverlap] {
		t.Fatalf("unexpected chunk boundaries: lengths %d, %d", len(chunks[0]), len(chunks[1]))
	}
	if chunks := splitChunks(" \n\t "); len(chunks) != 0 {
		t.Fatalf("expected no chunks for whitespace, got %v", chunks)
	}
}

func TestIngestPathRejectsUnsupportedUnsafeAndOversizedInputs(t *testing.T) {
	directory := t.TempDir()
	unsupported := filepath.Join(directory, "image.png")
	if err := os.WriteFile(unsupported, []byte("not text"), 0600); err != nil {
		t.Fatal(err)
	}
	large := filepath.Join(directory, "large.txt")
	if err := os.WriteFile(large, []byte(strings.Repeat("x", maxDocumentBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link.txt")
	if err := os.Symlink(large, link); err != nil {
		t.Fatal(err)
	}
	store := &Store{}
	for path, want := range map[string]string{
		unsupported: "only .md and .txt",
		large:       "exceeds 1 MiB",
		link:        "symbolic links",
	} {
		if _, err := store.IngestPath(context.Background(), path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("path %q: expected %q error, got %v", path, want, err)
		}
	}
}
