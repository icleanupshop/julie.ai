package sessions

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStorePersistsSessionHistoryAndDeletes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(session.ID) != 36 {
		t.Fatalf("session ID=%q", session.ID)
	}
	if _, err := store.Append(ctx, session.ID, "first", "answer one"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ctx, session.ID, "second", "answer two"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	history, err := store.History(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	rolesAndContent := make([]string, 0, len(history))
	for _, message := range history {
		rolesAndContent = append(rolesAndContent, message.Role+":"+message.Content)
	}
	want := []string{"user:first", "assistant:answer one", "user:second", "assistant:answer two"}
	if !reflect.DeepEqual(rolesAndContent, want) {
		t.Fatalf("history=%v, want %v", rolesAndContent, want)
	}
	if err := store.Delete(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.History(ctx, session.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("history after delete error=%v, want ErrNotFound", err)
	}
}

func TestStoreRejectsUnknownSession(t *testing.T) {
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Append(context.Background(), "missing", "prompt", "answer"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("append error=%v, want ErrNotFound", err)
	}
}
