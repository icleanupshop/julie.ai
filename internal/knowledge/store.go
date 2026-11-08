package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type EmbeddingConfig struct {
	URL   string `json:"url"`
	Model string `json:"model"`
}

type VectorStoreConfig struct {
	URL        string `json:"url"`
	Collection string `json:"collection"`
}

type Result struct {
	Source string  `json:"source"`
	Title  string  `json:"title,omitempty"`
	Text   string  `json:"text"`
	Score  float64 `json:"score"`
}

type Store struct {
	embeddingURL   string
	embeddingModel string
	apiKey         string
	qdrantURL      string
	collection     string
	http           *http.Client
	mu             sync.Mutex
	vectorSize     int
}

const maxConversationMemoryRunes = 32 << 10

func NewStore(embedding EmbeddingConfig, vectors VectorStoreConfig, apiKey string) (*Store, error) {
	if embedding.URL == "" && vectors.URL == "" {
		return nil, nil
	}
	if embedding.URL == "" || embedding.Model == "" || vectors.URL == "" {
		return nil, fmt.Errorf("embeddings URL/model and vector store URL must all be configured")
	}
	if err := validateEndpoint(embedding.URL, "embeddings"); err != nil {
		return nil, err
	}
	if err := validateEndpoint(vectors.URL, "vector store"); err != nil {
		return nil, err
	}
	collection := vectors.Collection
	if collection == "" {
		collection = "julie_knowledge"
	}
	for _, char := range collection {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-') {
			return nil, fmt.Errorf("vector collection may contain only letters, digits, underscores, and hyphens")
		}
	}
	return &Store{
		embeddingURL:   embedding.URL,
		embeddingModel: embedding.Model,
		apiKey:         apiKey,
		qdrantURL:      strings.TrimRight(vectors.URL, "/"),
		collection:     collection,
		http:           &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func validateEndpoint(raw, name string) error {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return fmt.Errorf("invalid %s endpoint", name)
	}
	return nil
}

func (store *Store) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	vector, err := store.embed(ctx, query)
	if err != nil {
		return nil, err
	}
	if err := store.ensureCollection(ctx, len(vector)); err != nil {
		return nil, err
	}
	request := struct {
		Vector      []float64 `json:"vector"`
		Limit       int       `json:"limit"`
		WithPayload bool      `json:"with_payload"`
	}{Vector: vector, Limit: limit, WithPayload: true}
	var response struct {
		Result []struct {
			Score   float64                    `json:"score"`
			Payload map[string]json.RawMessage `json:"payload"`
		} `json:"result"`
	}
	if err := store.qdrant(ctx, http.MethodPost, "/points/search", request, &response); err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(response.Result))
	for _, point := range response.Result {
		var result Result
		result.Score = point.Score
		_ = json.Unmarshal(point.Payload["source"], &result.Source)
		_ = json.Unmarshal(point.Payload["title"], &result.Title)
		_ = json.Unmarshal(point.Payload["text"], &result.Text)
		if result.Source != "" && result.Text != "" {
			results = append(results, result)
		}
	}
	return results, nil
}

func (store *Store) SearchMemory(ctx context.Context, query string, limit int) ([]Result, error) {
	return store.Search(ctx, query, limit)
}

func (store *Store) RememberConversation(ctx context.Context, sessionID string, turnID int64, prompt, answer string) error {
	if strings.TrimSpace(sessionID) == "" || turnID < 1 || strings.TrimSpace(prompt) == "" || strings.TrimSpace(answer) == "" {
		return fmt.Errorf("session, turn, prompt, and answer are required for conversation memory")
	}
	text := "User: " + prompt + "\nJulie: " + answer
	runes := []rune(text)
	if len(runes) > maxConversationMemoryRunes {
		text = string(runes[:maxConversationMemoryRunes])
	}
	chunks := splitChunks(text)
	points := make([]point, 0, len(chunks))
	source := "conversation/" + sessionID
	for index, chunk := range chunks {
		vector, err := store.embed(ctx, chunk)
		if err != nil {
			return fmt.Errorf("embed conversation memory: %w", err)
		}
		if err := store.ensureCollection(ctx, len(vector)); err != nil {
			return err
		}
		points = append(points, point{
			ID:     pointID(fmt.Sprintf("%s/%d", source, turnID), index, chunk),
			Vector: vector,
			Payload: map[string]any{
				"source":     source,
				"session_id": sessionID,
				"turn_id":    turnID,
				"text":       chunk,
			},
		})
	}
	for start := 0; start < len(points); start += upsertBatchSize {
		end := min(start+upsertBatchSize, len(points))
		if err := store.qdrant(ctx, http.MethodPut, "/points?wait=true", map[string]any{"points": points[start:end]}, nil); err != nil {
			return fmt.Errorf("store conversation memory: %w", err)
		}
	}
	return nil
}

func (store *Store) DeleteConversation(ctx context.Context, sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("session ID is required")
	}
	request := map[string]any{"filter": map[string]any{
		"must": []any{map[string]any{"key": "session_id", "match": map[string]string{"value": sessionID}}},
	}}
	err := store.qdrant(ctx, http.MethodPost, "/points/delete?wait=true", request, nil)
	if err != nil && err.Error() == "vector store returned HTTP 404" {
		return nil
	}
	return err
}

func (store *Store) embed(ctx context.Context, text string) ([]float64, error) {
	body, err := json.Marshal(map[string]string{"model": store.embeddingModel, "input": text})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, store.embeddingURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	if store.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+store.apiKey)
	}
	response, err := store.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call embeddings endpoint: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil {
		return nil, fmt.Errorf("read embeddings response: %w", err)
	}
	if len(data) > 2<<20 {
		return nil, fmt.Errorf("embeddings response exceeds 2 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("embeddings endpoint returned HTTP %d", response.StatusCode)
	}
	var result struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &result); err != nil || len(result.Data) != 1 || len(result.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embeddings endpoint returned an invalid vector")
	}
	for _, value := range result.Data[0].Embedding {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("embeddings endpoint returned a non-finite vector value")
		}
	}
	return result.Data[0].Embedding, nil
}

func (store *Store) ensureCollection(ctx context.Context, size int) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.vectorSize != 0 {
		if store.vectorSize != size {
			return fmt.Errorf("embedding size changed from %d to %d", store.vectorSize, size)
		}
		return nil
	}
	collectionSize, err := store.getCollectionSize(ctx)
	if err == nil {
		return store.setCollectionSize(size, collectionSize)
	}
	if err.Error() != "vector store returned HTTP 404" {
		return err
	}
	request := struct {
		Vectors struct {
			Size     int    `json:"size"`
			Distance string `json:"distance"`
		} `json:"vectors"`
	}{}
	request.Vectors.Size = size
	request.Vectors.Distance = "Cosine"
	if err := store.qdrant(ctx, http.MethodPut, "", request, nil); err != nil {
		if err.Error() != "vector store returned HTTP 409" {
			return err
		}
		collectionSize, err = store.getCollectionSize(ctx)
		if err != nil {
			return err
		}
	} else {
		collectionSize = size
	}
	return store.setCollectionSize(size, collectionSize)
}

func (store *Store) getCollectionSize(ctx context.Context) (int, error) {
	var response struct {
		Result struct {
			Config struct {
				Params struct {
					Vectors struct {
						Size int `json:"size"`
					} `json:"vectors"`
				} `json:"params"`
			} `json:"config"`
		} `json:"result"`
	}
	if err := store.qdrant(ctx, http.MethodGet, "", nil, &response); err != nil {
		return 0, err
	}
	if response.Result.Config.Params.Vectors.Size < 1 {
		return 0, fmt.Errorf("vector store returned an invalid collection configuration")
	}
	return response.Result.Config.Params.Vectors.Size, nil
}

func (store *Store) setCollectionSize(expected, actual int) error {
	if expected != actual {
		return fmt.Errorf("embedding size %d does not match vector collection size %d", expected, actual)
	}
	store.vectorSize = actual
	return nil
}

func (store *Store) qdrant(ctx context.Context, method, path string, body, output any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	endpoint := store.qdrantURL + "/collections/" + url.PathEscape(store.collection) + path
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := store.http.Do(request)
	if err != nil {
		return fmt.Errorf("call vector store: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil {
		return fmt.Errorf("read vector store response: %w", err)
	}
	if len(data) > 4<<20 {
		return fmt.Errorf("vector store response exceeds 4 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("vector store returned HTTP %d", response.StatusCode)
	}
	if output != nil && len(data) > 0 {
		if err := json.Unmarshal(data, output); err != nil {
			return fmt.Errorf("decode vector store response: %w", err)
		}
	}
	return nil
}
