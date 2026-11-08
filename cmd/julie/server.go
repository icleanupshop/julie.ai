package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func newHTTPHandler(token string, timeout time.Duration, ask func(context.Context, string) (string, error)) http.Handler {
	mux := http.NewServeMux()
	slots := make(chan struct{}, 8)
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("ok\n"))
	})
	mux.HandleFunc("POST /ask", func(writer http.ResponseWriter, request *http.Request) {
		if token == "" || subtle.ConstantTimeCompare([]byte(request.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			writer.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			http.Error(writer, "too many concurrent requests", http.StatusTooManyRequests)
			return
		}
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
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF || strings.TrimSpace(payload.Prompt) == "" {
			http.Error(writer, "provide one JSON object with a nonempty prompt", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(request.Context(), timeout)
		defer cancel()
		answer, err := ask(ctx, payload.Prompt)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Julie request:", err)
			status := http.StatusBadGateway
			if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
			}
			http.Error(writer, "agent request failed", status)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]string{"answer": answer})
	})
	return mux
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
