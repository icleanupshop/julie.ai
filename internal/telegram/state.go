package telegram

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

type SQLiteState struct {
	db *sql.DB
}

func OpenSQLiteState(ctx context.Context, path string) (*SQLiteState, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open Telegram state database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 5000; PRAGMA journal_mode = WAL;`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("configure Telegram state database: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS telegram_sessions (
			user_id INTEGER PRIMARY KEY,
			session_id TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS telegram_state (
			key TEXT PRIMARY KEY,
			value INTEGER NOT NULL
		);
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize Telegram state database: %w", err)
	}
	return &SQLiteState{db: db}, nil
}

func (state *SQLiteState) Close() error {
	return state.db.Close()
}

func (state *SQLiteState) SessionForUser(ctx context.Context, userID int64) (string, bool, error) {
	var sessionID string
	err := state.db.QueryRowContext(ctx, `SELECT session_id FROM telegram_sessions WHERE user_id = ?`, userID).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read Telegram session mapping: %w", err)
	}
	return sessionID, true, nil
}

func (state *SQLiteState) SaveSession(ctx context.Context, userID int64, sessionID string) error {
	if userID < 1 || sessionID == "" {
		return fmt.Errorf("valid Telegram user and Julie session are required")
	}
	if _, err := state.db.ExecContext(ctx, `INSERT INTO telegram_sessions(user_id, session_id) VALUES(?, ?) ON CONFLICT(user_id) DO NOTHING`, userID, sessionID); err != nil {
		return fmt.Errorf("save Telegram session mapping: %w", err)
	}
	return nil
}

func (state *SQLiteState) LastUpdateID(ctx context.Context) (int64, error) {
	var updateID int64
	err := state.db.QueryRowContext(ctx, `SELECT value FROM telegram_state WHERE key = 'last_update_id'`).Scan(&updateID)
	if errors.Is(err, sql.ErrNoRows) {
		return -1, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read Telegram update checkpoint: %w", err)
	}
	return updateID, nil
}

func (state *SQLiteState) SetLastUpdateID(ctx context.Context, updateID int64) error {
	if updateID < 0 {
		return fmt.Errorf("Telegram update ID must not be negative")
	}
	if _, err := state.db.ExecContext(ctx, `INSERT INTO telegram_state(key, value) VALUES('last_update_id', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, updateID); err != nil {
		return fmt.Errorf("save Telegram update checkpoint: %w", err)
	}
	return nil
}
