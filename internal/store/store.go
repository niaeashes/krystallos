// Package store holds the event log, which is the source of truth, and the
// projection tables derived from it.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// GenesisHash is the prev_hash of the first event in the log.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// Actor is the identity an event is signed with.
type Actor struct {
	Identity  string
	Principal string
	Session   string
}

// Scope places an event in an org and, optionally, a repository. It decides
// who may see the event on the event stream.
type Scope struct {
	Org  string
	Repo string
}

// Event is one entry of the hash-chained log.
type Event struct {
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	Version   int             `json:"version"`
	Org       string          `json:"org,omitempty"`
	Repo      string          `json:"repo,omitempty"`
	Identity  string          `json:"identity"`
	Principal string          `json:"principal"`
	Session   string          `json:"session,omitempty"`
	Payload   json.RawMessage `json:"payload"`
	At        string          `json:"at"`
	PrevHash  string          `json:"prev_hash"`
	Hash      string          `json:"hash"`
}

// ComputeHash returns the hash an event should carry. Subscribers can use it
// to verify the chain they received.
func (e *Event) ComputeHash() string {
	h := sha256.New()
	fmt.Fprintf(h, "krystallos-event-v1\n%d\n%s\n%d\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n",
		e.Seq, e.Type, e.Version, e.Org, e.Repo, e.Identity, e.Principal, e.Session, e.At, e.PrevHash)
	h.Write(e.Payload)
	return hex.EncodeToString(h.Sum(nil))
}

// Store wraps two connection pools on the same SQLite file: a single writer,
// which gives every event one total order, and a pool of readers.
type Store struct {
	w, r *sql.DB
	wmu  sync.Mutex

	smu  sync.Mutex
	subs map[chan struct{}]struct{}
}

// Open opens (and migrates) the database at path.
func Open(path string) (*Store, error) {
	dsn := "file:" + path +
		"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	if err := migrate(w); err != nil {
		w.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(8)
	return &Store{w: w, r: r, subs: map[chan struct{}]struct{}{}}, nil
}

// ErrOldDataDir means the database was made by a version of krystallos whose
// identity model this one no longer reads.
var ErrOldDataDir = errors.New("this data directory was made by an earlier krystallos with a different identity model; create a new one with `kry admin init`")

// migrate creates missing tables. Data directories from before identities
// became "kind:name/label" strings are refused rather than half-read.
func migrate(db *sql.DB) error {
	var oldPrincipals int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('principals') WHERE name = 'handle' OR name = 'name'`).Scan(&oldPrincipals); err != nil {
		return err
	}
	var hasID int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('principals') WHERE name = 'id'`).Scan(&hasID); err != nil {
		return err
	}
	if oldPrincipals > 0 && hasID == 0 {
		return ErrOldDataDir
	}
	_, err := db.Exec(schema)
	return err
}

// Meta reads a server-wide setting.
func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.r.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// Close closes both pools.
func (s *Store) Close() error {
	rerr := s.r.Close()
	if err := s.w.Close(); err != nil {
		return err
	}
	return rerr
}

// DB returns the reader pool.
func (s *Store) DB() *sql.DB { return s.r }

// Tx is a write transaction. Events appended through it are committed
// together with the projection changes made through the embedded *sql.Tx.
type Tx struct {
	*sql.Tx
	ctx    context.Context
	Actor  Actor
	Events []Event
}

// Write runs fn in a single write transaction signed by actor.
func (s *Store) Write(ctx context.Context, actor Actor, fn func(*Tx) error) ([]Event, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	sqlTx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	t := &Tx{Tx: sqlTx, ctx: ctx, Actor: actor}
	if err := fn(t); err != nil {
		sqlTx.Rollback()
		return nil, err
	}
	if err := sqlTx.Commit(); err != nil {
		return nil, err
	}
	if len(t.Events) > 0 {
		s.notify()
	}
	return t.Events, nil
}

// Append adds an event signed by the transaction's actor.
func (t *Tx) Append(typ string, scope Scope, payload any) (Event, error) {
	return t.AppendAs(t.Actor, typ, scope, payload)
}

// AppendAs adds an event signed by another actor, for the few cases where one
// request produces events from two identities.
func (t *Tx) AppendAs(actor Actor, typ string, scope Scope, payload any) (Event, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}
	e := Event{
		Type: typ, Version: 1, Org: scope.Org, Repo: scope.Repo,
		Identity: actor.Identity, Principal: actor.Principal, Session: actor.Session,
		Payload: b, At: time.Now().UTC().Format(time.RFC3339Nano),
	}
	err = t.QueryRowContext(t.ctx, `SELECT seq, hash FROM events ORDER BY seq DESC LIMIT 1`).Scan(&e.Seq, &e.PrevHash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		e.Seq, e.PrevHash = 0, GenesisHash
	case err != nil:
		return Event{}, err
	}
	e.Seq++
	e.Hash = e.ComputeHash()
	_, err = t.ExecContext(t.ctx,
		`INSERT INTO events (seq, type, version, org, repo, identity, principal, session, payload, at, prev_hash, hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Seq, e.Type, e.Version, e.Org, e.Repo, e.Identity, e.Principal, e.Session, string(e.Payload), e.At, e.PrevHash, e.Hash)
	if err != nil {
		return Event{}, err
	}
	t.Events = append(t.Events, e)
	return e, nil
}

// Events returns up to limit events with seq greater than after.
func (s *Store) Events(ctx context.Context, after int64, limit int) ([]Event, error) {
	rows, err := s.r.QueryContext(ctx,
		`SELECT seq, type, version, org, repo, identity, principal, session, payload, at, prev_hash, hash
		 FROM events WHERE seq > ? ORDER BY seq LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var payload string
		if err := rows.Scan(&e.Seq, &e.Type, &e.Version, &e.Org, &e.Repo, &e.Identity, &e.Principal, &e.Session, &payload, &e.At, &e.PrevHash, &e.Hash); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Verify walks the whole log and checks that every event is intact and
// chained to the one before it. It returns the number of events checked.
func (s *Store) Verify(ctx context.Context) (int64, error) {
	prev, last := GenesisHash, int64(0)
	for {
		evs, err := s.Events(ctx, last, 1000)
		if err != nil {
			return last, err
		}
		if len(evs) == 0 {
			return last, nil
		}
		for _, e := range evs {
			switch {
			case e.Seq != last+1:
				return last, fmt.Errorf("event %d: expected seq %d", e.Seq, last+1)
			case e.PrevHash != prev:
				return last, fmt.Errorf("event %d: prev_hash does not match event %d", e.Seq, last)
			case e.ComputeHash() != e.Hash:
				return last, fmt.Errorf("event %d: hash does not match its content", e.Seq)
			}
			prev, last = e.Hash, e.Seq
		}
	}
}

// Subscribe returns a channel that receives a signal after each committed
// write that appended events.
func (s *Store) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.smu.Lock()
	s.subs[ch] = struct{}{}
	s.smu.Unlock()
	return ch, func() {
		s.smu.Lock()
		delete(s.subs, ch)
		s.smu.Unlock()
	}
}

func (s *Store) notify() {
	s.smu.Lock()
	defer s.smu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
