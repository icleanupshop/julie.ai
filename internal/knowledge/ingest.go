package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxDocumentBytes = 1 << 20
	maxDocuments     = 200
	chunkRunes       = 1200
	chunkOverlap     = 200
	upsertBatchSize  = 32
)

type point struct {
	ID      string         `json:"id"`
	Vector  []float64      `json:"vector"`
	Payload map[string]any `json:"payload"`
}

func (store *Store) IngestPath(ctx context.Context, path string) (int, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, fmt.Errorf("inspect input: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return 0, fmt.Errorf("symbolic links are not accepted for ingestion")
	}
	if info.IsDir() {
		files := make([]string, 0)
		err := filepath.WalkDir(path, func(filePath string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if entry.IsDir() || !supportedDocument(filePath) {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			if len(files) >= maxDocuments {
				return fmt.Errorf("directory contains more than %d supported documents", maxDocuments)
			}
			files = append(files, filePath)
			return nil
		})
		if err != nil {
			return 0, fmt.Errorf("walk input directory: %w", err)
		}
		total := 0
		for _, filePath := range files {
			relative, err := filepath.Rel(path, filePath)
			if err != nil {
				return total, err
			}
			count, err := store.ingestFile(ctx, filePath, filepath.ToSlash(relative))
			if err != nil {
				return total, fmt.Errorf("ingest %s: %w", relative, err)
			}
			total += count
		}
		return total, nil
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("input must be a regular file or directory")
	}
	if !supportedDocument(path) {
		return 0, fmt.Errorf("only .md and .txt files are supported")
	}
	return store.ingestFile(ctx, path, filepath.Base(path))
}

func supportedDocument(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".txt":
		return true
	default:
		return false
	}
}

func (store *Store) ingestFile(ctx context.Context, path, source string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxDocumentBytes+1))
	if err != nil {
		return 0, err
	}
	if len(data) > maxDocumentBytes {
		return 0, fmt.Errorf("document exceeds 1 MiB")
	}
	if !utf8.Valid(data) {
		return 0, fmt.Errorf("document is not valid UTF-8 text")
	}
	text := strings.TrimSpace(string(data))
	chunks := splitChunks(text)
	if len(chunks) == 0 {
		return 0, nil
	}
	hash := sha256.Sum256(data)
	contentHash := hex.EncodeToString(hash[:])
	points := make([]point, 0, len(chunks))
	for index, chunk := range chunks {
		vector, err := store.embed(ctx, chunk)
		if err != nil {
			return 0, err
		}
		if err := store.ensureCollection(ctx, len(vector)); err != nil {
			return 0, err
		}
		points = append(points, point{
			ID:     pointID(source, index, chunk),
			Vector: vector,
			Payload: map[string]any{
				"source":       source,
				"title":        filepath.Base(path),
				"text":         chunk,
				"chunk_index":  index,
				"content_hash": contentHash,
			},
		})
	}
	upsert := func(batch []point) error {
		request := map[string]any{"points": batch}
		return store.qdrant(ctx, http.MethodPut, "/points?wait=true", request, nil)
	}
	for start := 0; start < len(points); start += upsertBatchSize {
		end := min(start+upsertBatchSize, len(points))
		if err := upsert(points[start:end]); err != nil {
			return start, err
		}
	}
	deleteRequest := map[string]any{"filter": map[string]any{
		"must": []any{
			map[string]any{"key": "source", "match": map[string]string{"value": source}},
		},
		"must_not": []any{
			map[string]any{"key": "content_hash", "match": map[string]string{"value": contentHash}},
		},
	}}
	if err := store.qdrant(ctx, http.MethodPost, "/points/delete?wait=true", deleteRequest, nil); err != nil {
		return 0, err
	}
	return len(points), nil
}

func splitChunks(text string) []string {
	runes := []rune(text)
	if len(runes) == 0 {
		return nil
	}
	chunks := make([]string, 0, (len(runes)+chunkRunes-1)/(chunkRunes-chunkOverlap))
	for start := 0; start < len(runes); {
		end := min(start+chunkRunes, len(runes))
		if end < len(runes) {
			for boundary := end; boundary > start+chunkRunes/2; boundary-- {
				if unicode.IsSpace(runes[boundary-1]) {
					end = boundary
					break
				}
			}
		}
		chunk := strings.TrimSpace(string(runes[start:end]))
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
		if end == len(runes) {
			break
		}
		start = end - chunkOverlap
	}
	return chunks
}

func pointID(source string, index int, text string) string {
	hash := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%s", source, index, text))
	identifier := hash[:16]
	identifier[6] = identifier[6]&0x0f | 0x50
	identifier[8] = identifier[8]&0x3f | 0x80
	encoded := hex.EncodeToString(identifier)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}
