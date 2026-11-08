package sessions

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("session not found")

type Session struct {
	ID        string    `json:"session_id"`
	CreatedAt time.Time `json:"created_at"`
}

type Message struct {
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sessions database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON; PRAGMA busy_timeout = 5000; PRAGMA journal_mode = WAL;`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("configure sessions database: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			created_at TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS turns (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			prompt TEXT NOT NULL,
			answer TEXT NOT NULL,
			created_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS turns_session_id_id ON turns(session_id, id);
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize sessions database: %w", err)
	}
	return &Store{db: db}, nil
}

func (store *Store) Close() error {
	return store.db.Close()
}

func (store *Store) Create(ctx context.Context) (Session, error) {
	id, err := newID()
	if err != nil {
		return Session{}, err
	}
	session := Session{ID: id, CreatedAt: time.Now().UTC()}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO sessions(id, created_at) VALUES(?, ?)`, session.ID, session.CreatedAt.Format(time.RFC3339Nano)); err != nil {
		return Session{}, fmt.Errorf("create session: %w", err)
	}
	return session, nil
}

func (store *Store) History(ctx context.Context, sessionID string) ([]Message, error) {
	var exists int
	if err := store.db.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, sessionID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read session: %w", err)
	}
	rows, err := store.db.QueryContext(ctx, `SELECT prompt, answer, created_at FROM turns WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("read session history: %w", err)
	}
	defer rows.Close()
	history := make([]Message, 0)
	for rows.Next() {
		var prompt, answer, timestamp string
		if err := rows.Scan(&prompt, &answer, &timestamp); err != nil {
			return nil, fmt.Errorf("decode session history: %w", err)
		}
		createdAt, err := time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			return nil, fmt.Errorf("decode session timestamp: %w", err)
		}
		history = append(history, Message{Role: "user", Content: prompt, CreatedAt: createdAt}, Message{Role: "assistant", Content: answer, CreatedAt: createdAt})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read session history: %w", err)
	}
	return history, nil
}

func (store *Store) Append(ctx context.Context, sessionID, prompt, answer string) (int64, error) {
	createdAt := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := store.db.ExecContext(ctx, `INSERT INTO turns(session_id, prompt, answer, created_at) SELECT id, ?, ?, ? FROM sessions WHERE id = ?`, prompt, answer, createdAt, sessionID)
	if err != nil {
		return 0, fmt.Errorf("append session turn: %w", err)
	}
	turnID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read session turn ID: %w", err)
	}
	if turnID == 0 {
		return 0, ErrNotFound
	}
	return turnID, nil
}

func (store *Store) Delete(ctx context.Context, sessionID string) error {
	result, err := store.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, sessionID)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check deleted session: %w", err)
	}
	if deleted == 0 {
		return ErrNotFound
	}
	return nil
}

func newID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate session ID: %w", err)
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16]), nil
}
