package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Message is one stored message, incoming or outgoing.
type Message struct {
	ID         string `json:"id"`
	From       string `json:"from"`
	Body       string `json:"body"`
	Card       string `json:"card,omitempty"`
	ReceivedAt string `json:"received_at"`
}

// Store is the relay's SQLite message log.
type Store struct{ db *sql.DB }

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS messages (
		id TEXT PRIMARY KEY,
		direction TEXT NOT NULL CHECK (direction IN ('in', 'out')),
		peer TEXT NOT NULL,
		login TEXT,
		body TEXT NOT NULL,
		created_at TEXT NOT NULL,
		read_at TEXT
	)`)
	if err != nil {
		return nil, fmt.Errorf("init schema: %w", err)
	}
	// stores made before messages could be about a card
	var hasCard int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('messages') WHERE name = 'card'`).Scan(&hasCard); err != nil {
		return nil, fmt.Errorf("read schema: %w", err)
	}
	if hasCard == 0 {
		if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN card TEXT`); err != nil {
			return nil, fmt.Errorf("add card column: %w", err)
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// Insert stores a message once; it reports false when the id already exists (a retried delivery).
// card is the Trame card the message is about, or "".
func (s *Store) Insert(ctx context.Context, id, direction, peer, body, login, card string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO messages (id, direction, peer, login, body, card, created_at) VALUES (?, ?, ?, ?, ?, NULLIF(?, ''), ?)`,
		id, direction, peer, login, body, card, now())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// TakeInbox returns incoming messages oldest first, only those about `card` when it is set, and marks the unread ones read.
func (s *Store) TakeInbox(ctx context.Context, includeRead bool, limit int, card string) ([]Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	where := "direction = 'in'"
	if !includeRead {
		where += " AND read_at IS NULL"
	}
	args := []any{}
	if card != "" {
		where += " AND card = ?"
		args = append(args, card)
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id, peer, body, coalesce(card, ''), created_at, read_at IS NULL FROM messages WHERE `+where+` ORDER BY created_at LIMIT ?`,
		append(args, limit)...)
	if err != nil {
		return nil, err
	}
	msgs := []Message{}
	var unread []any
	for rows.Next() {
		var m Message
		var isUnread bool
		if err := rows.Scan(&m.ID, &m.From, &m.Body, &m.Card, &m.ReceivedAt, &isUnread); err != nil {
			rows.Close()
			return nil, err
		}
		msgs = append(msgs, m)
		if isUnread {
			unread = append(unread, m.ID)
		}
	}
	rows.Close()
	if len(unread) > 0 {
		q := `UPDATE messages SET read_at = ? WHERE id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(unread)), ",") + `)`
		if _, err := tx.ExecContext(ctx, q, append([]any{now()}, unread...)...); err != nil {
			return nil, err
		}
	}
	return msgs, tx.Commit()
}
