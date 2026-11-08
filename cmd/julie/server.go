package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"julie.ai/internal/sessions"
)

type sessionRepository interface {
	Create(context.Context) (sessions.Session, error)
	History(context.Context, string) ([]sessions.Message, error)
	Append(context.Context, string, string, string) (int64, error)
	Delete(context.Context, string) error
}

func newHTTPHandler(timeout time.Duration, sessionStore sessionRepository,
	ask func(context.Context, string, []sessions.Message, string) (string, error),
	remember func(context.Context, string, int64, string, string) error,
	forget func(context.Context, string) error,
) http.Handler {
	mux := http.NewServeMux()
	slots := make(chan struct{}, 8)
	acquire := func(writer http.ResponseWriter) bool {
		select {
		case slots <- struct{}{}:
			return true
		default:
			http.Error(writer, "too many concurrent requests", http.StatusTooManyRequests)
			return false
		}
	}
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("ok\n"))
	})
	mux.HandleFunc("POST /ask", func(writer http.ResponseWriter, request *http.Request) {
		if !acquire(writer) {
			return
		}
		defer func() { <-slots }()
		ctx, cancel := context.WithTimeout(request.Context(), timeout)
		defer cancel()
		prompt, ok := decodePrompt(writer, request)
		if !ok {
			return
		}
		answer, err := ask(ctx, "", nil, prompt)
		if err != nil {
			writeAgentError(writer, ctx, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]string{"answer": answer})
	})
	mux.HandleFunc("POST /sessions", func(writer http.ResponseWriter, request *http.Request) {
		request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
		defer request.Body.Close()
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, "invalid request body", http.StatusBadRequest)
			return
		}
		if len(body) != 0 {
			http.Error(writer, "session creation does not accept a request body", http.StatusBadRequest)
			return
		}
		session, err := sessionStore.Create(request.Context())
		if err != nil {
			http.Error(writer, "could not create session", http.StatusInternalServerError)
			return
		}
		writeJSON(writer, http.StatusCreated, session)
	})
	mux.HandleFunc("GET /sessions/{sessionID}", func(writer http.ResponseWriter, request *http.Request) {
		history, err := sessionStore.History(request.Context(), request.PathValue("sessionID"))
		if errors.Is(err, sessions.ErrNotFound) {
			http.NotFound(writer, request)
			return
		}
		if err != nil {
			http.Error(writer, "could not read session", http.StatusInternalServerError)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"session_id": request.PathValue("sessionID"), "messages": history})
	})
	mux.HandleFunc("POST /sessions/{sessionID}/messages", func(writer http.ResponseWriter, request *http.Request) {
		if !acquire(writer) {
			return
		}
		defer func() { <-slots }()
		ctx, cancel := context.WithTimeout(request.Context(), timeout)
		defer cancel()
		sessionID := request.PathValue("sessionID")
		history, err := sessionStore.History(ctx, sessionID)
		if errors.Is(err, sessions.ErrNotFound) {
			http.NotFound(writer, request)
			return
		}
		if err != nil {
			http.Error(writer, "could not read session", http.StatusInternalServerError)
			return
		}
		prompt, ok := decodePrompt(writer, request)
		if !ok {
			return
		}
		answer, err := ask(ctx, sessionID, history, prompt)
		if err != nil {
			writeAgentError(writer, ctx, err)
			return
		}
		turnID, err := sessionStore.Append(ctx, sessionID, prompt, answer)
		if err != nil {
			http.Error(writer, "could not save session turn", http.StatusInternalServerError)
			return
		}
		if remember != nil {
			if err := remember(ctx, sessionID, turnID, prompt, answer); err != nil {
				fmt.Fprintln(os.Stderr, "Julie conversation memory:", err)
			}
		}
		writeJSON(writer, http.StatusOK, map[string]string{"session_id": sessionID, "answer": answer})
	})
	mux.HandleFunc("DELETE /sessions/{sessionID}", func(writer http.ResponseWriter, request *http.Request) {
		sessionID := request.PathValue("sessionID")
		if forget != nil {
			if err := forget(request.Context(), sessionID); err != nil {
				http.Error(writer, "could not delete session memories", http.StatusBadGateway)
				return
			}
		}
		if err := sessionStore.Delete(request.Context(), sessionID); errors.Is(err, sessions.ErrNotFound) {
			http.NotFound(writer, request)
			return
		} else if err != nil {
			http.Error(writer, "could not delete session", http.StatusInternalServerError)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func decodePrompt(writer http.ResponseWriter, request *http.Request) (string, bool) {
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	defer request.Body.Close()
	var payload struct {
		Prompt string `json:"prompt"`
	}
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		var limitError *http.MaxBytesError
		if errors.As(err, &limitError) {
			http.Error(writer, "request exceeds 1 MiB", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(writer, "invalid JSON request", http.StatusBadRequest)
		}
		return "", false
	}
	if err := decoder.Decode(new(any)); err != io.EOF || strings.TrimSpace(payload.Prompt) == "" {
		http.Error(writer, "provide one JSON object with a nonempty prompt", http.StatusBadRequest)
		return "", false
	}
	return payload.Prompt, true
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeAgentError(writer http.ResponseWriter, ctx context.Context, err error) {
	fmt.Fprintln(os.Stderr, "Julie request:", err)
	status := http.StatusBadGateway
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
	}
	http.Error(writer, "agent request failed", status)
}

func serveHTTP(ctx context.Context, address string, handler http.Handler, timeout time.Duration) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      timeout + 15*time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	fmt.Fprintln(os.Stderr, "Julie HTTP API listening on", listener.Addr())
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout+5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return fmt.Errorf("shutdown HTTP server: %w", err)
		}
		return nil
	}
}
