package app

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// TestCeremoniesAreCapped fills the pending sign-ins to the cap: the next
// begin is refused with 503 until some of them expire.
func TestCeremoniesAreCapped(t *testing.T) {
	ctx := context.Background()
	a, err := Open(ctx, Config{DataDir: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	if err := a.EnablePasskeys(PasskeyConfig{RPID: "localhost", Origins: []string{"http://localhost:3000"}}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < maxCeremonies; i++ {
		if _, _, err := a.BeginPasskeyLogin(ctx); err != nil {
			t.Fatalf("sign-in %d of %d: %v", i+1, maxCeremonies, err)
		}
	}
	_, _, err = a.BeginPasskeyLogin(ctx)
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != http.StatusServiceUnavailable {
		t.Fatalf("a sign-in past the cap: got %v, want 503", err)
	}
	if n := len(a.pk.ceremonies); n != maxCeremonies {
		t.Fatalf("%d ceremonies pending, want the cap, %d", n, maxCeremonies)
	}

	// Let two of them lapse: they make room, and only they are dropped.
	a.pk.mu.Lock()
	expired := 0
	for id, c := range a.pk.ceremonies {
		if expired == 2 {
			break
		}
		c.expires = time.Now().Add(-time.Second)
		a.pk.ceremonies[id] = c
		expired++
	}
	a.pk.mu.Unlock()
	if _, _, err := a.BeginPasskeyLogin(ctx); err != nil {
		t.Fatalf("a sign-in after two expired: %v", err)
	}
	if n := len(a.pk.ceremonies); n != maxCeremonies-1 {
		t.Fatalf("%d ceremonies pending, want %d", n, maxCeremonies-1)
	}
}
