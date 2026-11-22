package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LocalAgent is one agent session on this machine, identified by its process.
type LocalAgent struct {
	Name      string `json:"name"`
	Client    string `json:"client"`
	PID       int    `json:"-"`
	ProcStart string `json:"-"`
	Cwd       string `json:"cwd"`
	TmuxPane  string `json:"-"`
	TmuxSock  string `json:"-"`
	State     string `json:"state"`
}

// LocalStore is the SQLite file every local agent session shares; WAL lets several processes use it at once.
type LocalStore struct {
	db   *sql.DB
	proc string // procfs root, swapped in tests
}

const (
	stateIdle = "idle"
	stateBusy = "busy"
)

// localStatePath resolves the store from getenv, so an agent's MCP server and hooks agree on it.
func localStatePath(getenv func(string) string) (string, error) {
	dir, err := stateRootFrom(getenv)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "local.db"), nil
}

// stateRoot is $AGENT_RELAY_HOME/state, else $XDG_STATE_HOME/agent-relay, else ~/.local/state/agent-relay.
func stateRoot() (string, error) { return stateRootFrom(os.Getenv) }

func stateRootFrom(getenv func(string) string) (string, error) {
	var dir string
	switch {
	case getenv("AGENT_RELAY_HOME") != "":
		dir = filepath.Join(getenv("AGENT_RELAY_HOME"), "state")
	case getenv("XDG_STATE_HOME") != "":
		dir = filepath.Join(getenv("XDG_STATE_HOME"), "agent-relay")
	default:
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".local", "state", "agent-relay")
	}
	return dir, os.MkdirAll(dir, 0o700)
}

func OpenLocalStore(path, proc string) (*LocalStore, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS agents (
		name TEXT PRIMARY KEY,
		client TEXT NOT NULL,
		pid INTEGER NOT NULL,
		proc_start TEXT NOT NULL,
		cwd TEXT NOT NULL,
		tmux_pane TEXT,
		tmux_sock TEXT,
		state TEXT NOT NULL CHECK (state IN ('idle', 'busy')),
		seen_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS local_messages (
		id TEXT PRIMARY KEY,
		sender TEXT NOT NULL,
		recipient TEXT NOT NULL,
		body TEXT NOT NULL,
		card TEXT,
		created_at TEXT NOT NULL,
		read_at TEXT,
		announced_at TEXT
	);
	CREATE INDEX IF NOT EXISTS local_messages_unread ON local_messages (recipient) WHERE read_at IS NULL;`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("init local schema: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, err
	}
	return &LocalStore{db: db, proc: proc}, nil
}

func (s *LocalStore) Close() error { return s.db.Close() }

// Register returns the name a's process already holds, else claims a.Name (or -2, -3, …), taking over a
// name held by a dead process.
func (s *LocalStore) Register(ctx context.Context, a LocalAgent) (string, error) {
	if held, err := s.ByProcess(ctx, a.PID, a.ProcStart); err != nil || held != nil {
		if held != nil {
			return held.Name, nil
		}
		return "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	for i := 1; i < 100; i++ {
		name := a.Name
		if i > 1 {
			name = fmt.Sprintf("%s-%d", a.Name, i)
		}
		var pid int
		var start string
		err := tx.QueryRowContext(ctx, `SELECT pid, proc_start FROM agents WHERE name = ?`, name).Scan(&pid, &start)
		if err == nil && !(pid == a.PID && start == a.ProcStart) && procAlive(s.proc, pid, start) {
			continue
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO agents
			(name, client, pid, proc_start, cwd, tmux_pane, tmux_sock, state, seen_at) VALUES (?, ?, ?, ?, ?, ?, ?, 'idle', ?)`,
			name, a.Client, a.PID, a.ProcStart, a.Cwd, a.TmuxPane, a.TmuxSock, now())
		if err != nil {
			return "", err
		}
		return name, tx.Commit()
	}
	return "", fmt.Errorf("no free name for %q", a.Name)
}

// ByProcess returns the registered agent running as pid, if any.
func (s *LocalStore) ByProcess(ctx context.Context, pid int, start string) (*LocalAgent, error) {
	rows, err := s.query(ctx, `WHERE pid = ? AND proc_start = ?`, pid, start)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

// Live returns the agents whose process is still running, by name.
func (s *LocalStore) Live(ctx context.Context) ([]LocalAgent, error) {
	all, err := s.query(ctx, `ORDER BY name`)
	if err != nil {
		return nil, err
	}
	live := all[:0]
	for _, a := range all {
		if procAlive(s.proc, a.PID, a.ProcStart) {
			live = append(live, a)
		}
	}
	return live, nil
}

func (s *LocalStore) query(ctx context.Context, tail string, args ...any) ([]LocalAgent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, client, pid, proc_start, cwd, coalesce(tmux_pane, ''),
		coalesce(tmux_sock, ''), state FROM agents `+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LocalAgent{}
	for rows.Next() {
		var a LocalAgent
		if err := rows.Scan(&a.Name, &a.Client, &a.PID, &a.ProcStart, &a.Cwd, &a.TmuxPane, &a.TmuxSock, &a.State); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *LocalStore) SetState(ctx context.Context, name, state string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET state = ?, seen_at = ? WHERE name = ?`, state, now(), name)
	return err
}

func (s *LocalStore) Send(ctx context.Context, id, from, to, body, card string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO local_messages (id, sender, recipient, body, card, created_at)
		VALUES (?, ?, ?, ?, nullif(?, ''), ?)`, id, from, to, body, card, now())
	return err
}

// Inbox returns name's messages oldest first and marks the unread ones read (and so announced).
func (s *LocalStore) Inbox(ctx context.Context, name string, includeRead bool, limit int, card string) ([]Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	where, args := "recipient = ?", []any{name}
	if !includeRead {
		where += " AND read_at IS NULL"
	}
	if card != "" {
		where += " AND card = ?"
		args = append(args, card)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, sender, body, coalesce(card, ''), created_at, read_at IS NULL
		FROM local_messages WHERE `+where+` ORDER BY created_at, rowid LIMIT ?`, append(args, limit)...)
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
		q := `UPDATE local_messages SET read_at = ?1, announced_at = coalesce(announced_at, ?1) WHERE id IN (` +
			strings.TrimSuffix(strings.Repeat("?,", len(unread)), ",") + `)`
		if _, err := tx.ExecContext(ctx, q, append([]any{now()}, unread...)...); err != nil {
			return nil, err
		}
	}
	return msgs, tx.Commit()
}

type claimMode int

const (
	claimIfIdle    claimMode = iota // a sender waking an agent that waits for input
	claimAtTurnEnd                  // the agent's end-of-turn hook
)

// Claim takes name's unannounced unread messages so each wakes the agent once and returns their senders.
// claimIfIdle claims nothing while the agent is mid-turn; claimAtTurnEnd marks the agent idle when there is
// nothing to claim. Either way a claim marks it busy, all in one transaction so no message falls between.
func (s *LocalStore) Claim(ctx context.Context, name string, mode claimMode) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if mode == claimIfIdle {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM agents WHERE name = ?`, name).Scan(&state); err != nil {
			return nil, err
		}
		if state != stateIdle {
			return nil, nil
		}
	}
	rows, err := tx.QueryContext(ctx, `UPDATE local_messages SET announced_at = ?
		WHERE recipient = ? AND read_at IS NULL AND announced_at IS NULL RETURNING sender`, now(), name)
	if err != nil {
		return nil, err
	}
	var senders []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, err
		}
		senders = append(senders, s)
	}
	rows.Close()
	state := stateBusy
	if len(senders) == 0 {
		if mode == claimIfIdle {
			return nil, tx.Commit()
		}
		state = stateIdle
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agents SET state = ?, seen_at = ? WHERE name = ?`, state, now(), name); err != nil {
		return nil, err
	}
	return senders, tx.Commit()
}

// Unclaim undoes a claim whose wake-up failed: the messages wait for the next send or end of turn.
func (s *LocalStore) Unclaim(ctx context.Context, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE local_messages SET announced_at = NULL WHERE recipient = ? AND read_at IS NULL`, name); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agents SET state = 'idle', seen_at = ? WHERE name = ?`, now(), name); err != nil {
		return err
	}
	return tx.Commit()
}
