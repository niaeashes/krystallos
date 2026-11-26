// Package accesslog records reads. Reads far outnumber writes, so they live
// in their own SQLite file and are written in batches, away from the event
// log's single writer.
package accesslog

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Entry is one read by one identity.
type Entry struct {
	At        string `json:"at"`
	Identity  string `json:"identity"`
	Principal string `json:"principal"`
	Session   string `json:"session,omitempty"`
	Kind      string `json:"kind"`
	Repo      string `json:"repo,omitempty"`
	Target    string `json:"target"`
	Detail    any    `json:"detail,omitempty"`
}

// Log batches entries into access.db.
type Log struct {
	db   *sql.DB
	ch   chan Entry
	done chan struct{}

	mu     sync.RWMutex
	closed bool
}

const schema = `
CREATE TABLE IF NOT EXISTS access (
	id        INTEGER PRIMARY KEY,
	at        TEXT NOT NULL,
	identity  TEXT NOT NULL,
	principal TEXT NOT NULL,
	session   TEXT NOT NULL,
	kind      TEXT NOT NULL,
	repo      TEXT NOT NULL,
	target    TEXT NOT NULL,
	detail    TEXT NOT NULL
);`

// Open opens access.db at path and starts the batch writer.
func Open(path string) (*Log, error) {
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	l := &Log{db: db, ch: make(chan Entry, 1024), done: make(chan struct{})}
	go l.run()
	return l, nil
}

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Record queues an entry. It blocks when the queue is full rather than drop
// a read.
func (l *Log) Record(e Entry) {
	if e.At == "" {
		e.At = time.Now().UTC().Format(time.RFC3339Nano)
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		slog.Warn("read arrived after the access log closed", "identity", e.Identity, "kind", e.Kind, "target", e.Target)
		return
	}
	l.ch <- e
}

// Close flushes the queue and closes the database.
func (l *Log) Close() error {
	l.mu.Lock()
	l.closed = true
	close(l.ch)
	l.mu.Unlock()
	<-l.done
	return l.db.Close()
}

func (l *Log) run() {
	defer close(l.done)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	var buf []Entry
	flush := func() {
		if len(buf) == 0 {
			return
		}
		if err := l.write(buf); err != nil {
			slog.Error("access log write failed", "err", err, "entries", len(buf))
		}
		buf = buf[:0]
	}
	for {
		select {
		case e, ok := <-l.ch:
			if !ok {
				flush()
				return
			}
			buf = append(buf, e)
			if len(buf) >= 256 {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

func (l *Log) write(entries []Entry) error {
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO access (at, identity, principal, session, kind, repo, target, detail) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range entries {
		detail := []byte("null")
		if e.Detail != nil {
			if detail, err = json.Marshal(e.Detail); err != nil {
				return err
			}
		}
		if _, err := stmt.Exec(e.At, e.Identity, e.Principal, e.Session, e.Kind, e.Repo, e.Target, string(detail)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Read returns the most recent entries, newest last, optionally only those of
// one identity. It opens the file on its own so the admin CLI can use it
// while the server runs.
func Read(ctx context.Context, path, identity string, limit int) ([]Entry, error) {
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	q := `SELECT at, identity, principal, session, kind, repo, target, detail FROM access`
	args := []any{}
	if identity != "" {
		q += ` WHERE identity = ?`
		args = append(args, identity)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var detail string
		if err := rows.Scan(&e.At, &e.Identity, &e.Principal, &e.Session, &e.Kind, &e.Repo, &e.Target, &detail); err != nil {
			return nil, err
		}
		e.Detail = json.RawMessage(detail)
		out = append(out, e)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}
