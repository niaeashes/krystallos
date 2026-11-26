package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyDetectsTampering(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	alice := Actor{Identity: "alice", Principal: "alice"}
	for i := range 3 {
		_, err := s.Write(ctx, alice, func(tx *Tx) error {
			_, err := tx.Append("test.event", Scope{Org: "acme"}, map[string]int{"n": i})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.Verify(ctx); err != nil || n != 3 {
		t.Fatalf("Verify = %d, %v; want 3, nil", n, err)
	}

	// Rewriting history, even a single field, breaks the chain.
	if _, err := s.w.Exec(`UPDATE events SET identity = 'mallory' WHERE seq = 2`); err != nil {
		t.Fatal(err)
	}
	_, err = s.Verify(ctx)
	if err == nil || !strings.Contains(err.Error(), "event 2") {
		t.Fatalf("Verify after tampering = %v; want an error naming event 2", err)
	}
}

func TestWriteRollsBackEventsWithProjection(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.Write(ctx, Actor{Identity: "alice", Principal: "alice"}, func(tx *Tx) error {
		if _, err := tx.Append("org.created", Scope{Org: "acme"}, nil); err != nil {
			return err
		}
		// The projection write fails, so the event must not survive either.
		_, err := tx.Exec(`INSERT INTO no_such_table VALUES (1)`)
		return err
	})
	if err == nil {
		t.Fatal("expected the write to fail")
	}
	evs, err := s.Events(ctx, 0, 10)
	if err != nil || len(evs) != 0 {
		t.Fatalf("events after a failed write = %d, %v; want none", len(evs), err)
	}
}
