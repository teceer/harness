package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var bgctx = context.Background()

// Tx is a read-modify-write view used inside Store.Tx.
type Tx struct{ c *sql.Conn }

func (t *Tx) Get(id string) (Session, bool, error) {
	se, err := scan(t.c.QueryRowContext(bgctx, `SELECT `+cols+` FROM sessions WHERE session_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	return se, err == nil, err
}

func (t *Tx) ByPane(pane string) ([]Session, error) {
	rows, err := t.c.QueryContext(bgctx, `SELECT `+cols+` FROM sessions WHERE tmux_pane = ?`, pane)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		se, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, se)
	}
	return out, rows.Err()
}

// Put inserts or fully replaces a row.
func (t *Tx) Put(s Session) error {
	now := time.Now()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = now
	}
	if s.StatusSince.IsZero() {
		s.StatusSince = now
	}
	var archived int64
	if !s.ArchivedAt.IsZero() {
		archived = s.ArchivedAt.Unix()
	}
	_, err := t.c.ExecContext(bgctx, `INSERT OR REPLACE INTO sessions (`+cols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.ID, s.Profile, s.ConfigDir, s.Cwd, s.TranscriptPath, s.Name, s.Title, s.Status, s.Detail,
		s.LastPrompt, s.LastMessage, s.TmuxPane, s.PID,
		s.CreatedAt.Unix(), s.UpdatedAt.Unix(), s.StatusSince.Unix(), archived)
	return err
}

func (t *Tx) Delete(id string) error {
	_, err := t.c.ExecContext(bgctx, `DELETE FROM sessions WHERE session_id = ?`, id)
	return err
}

// SetStatus changes the status, resetting StatusSince only on a real change.
func (s *Session) SetStatus(status string, now time.Time) {
	if s.Status != status {
		s.Status = status
		s.StatusSince = now
	}
}
