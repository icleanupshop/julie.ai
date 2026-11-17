package telegram

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSQLiteStatePersistsSessionsAndUpdateCheckpoint(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "telegram.db")
	state, err := OpenSQLiteState(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SaveSession(ctx, 42, "session-42"); err != nil {
		t.Fatal(err)
	}
	if err := state.SetLastUpdateID(ctx, 19); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLiteState(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	sessionID, ok, err := reopened.SessionForUser(ctx, 42)
	if err != nil || !ok || sessionID != "session-42" {
		t.Fatalf("session=%q found=%t err=%v", sessionID, ok, err)
	}
	updateID, err := reopened.LastUpdateID(ctx)
	if err != nil || updateID != 19 {
		t.Fatalf("update ID=%d err=%v", updateID, err)
	}
}
