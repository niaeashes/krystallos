// Package app holds krystallos's domain logic. Every change goes through
// store.Write, so it is recorded as an event signed by an identity in the same
// transaction that updates the projections.
package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"krystallos/internal/gitx"
	"krystallos/internal/ident"
	"krystallos/internal/store"
)

// Config is where the app keeps its data and which binaries it runs.
type Config struct {
	DataDir string
	GitBin  string
	// HookBin is the kry binary that repository hooks call back into.
	HookBin string
}

// App is the domain layer.
type App struct {
	cfg   Config
	Store *store.Store
	Git   gitx.Git

	opsMu sync.Mutex
	ops   map[string]Op

	pk *passkeys

	oauth oauthMem

	hostMu    sync.Mutex
	hostCache string

	internalURL    string
	internalSecret string
}

// SystemActor is the built-in service service:krystallos, which signs what the
// server does on its own (merge commits, reconciliation). UnattributedActor
// marks ref changes that reached a repository without going through the
// server; it is not an identity anyone holds.
var (
	SystemActor       = store.Actor{Identity: "service:krystallos", Principal: "service:krystallos"}
	UnattributedActor = store.Actor{Identity: ident.Unattributed, Principal: ident.Unattributed}
)

// ErrRenamedDataDir means the data directory was made when this server was
// called gitsynth. Its log cannot be verified under the new name.
var ErrRenamedDataDir = errors.New("this data directory was made by gitsynth, this server's earlier name, and cannot be read; create a new one with `kry admin init`")

// Open opens the data directory, creating it if needed.
func Open(ctx context.Context, cfg Config) (*App, error) {
	dataDir, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{dataDir, filepath.Join(dataDir, "repos"), filepath.Join(dataDir, "run")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	// Hooks report the repository by its resolved path, so resolve ours too.
	if dataDir, err = filepath.EvalSymlinks(dataDir); err != nil {
		return nil, err
	}
	cfg.DataDir = dataDir
	// A data directory from before the server was renamed holds gitsynth.db,
	// whose log is hashed under the old name. It is refused rather than
	// started over beside the old database.
	if _, err := os.Stat(filepath.Join(dataDir, "gitsynth.db")); err == nil {
		return nil, ErrRenamedDataDir
	}
	if cfg.GitBin == "" {
		cfg.GitBin = "git"
	}
	st, err := store.Open(filepath.Join(dataDir, "krystallos.db"))
	if err != nil {
		return nil, err
	}
	return &App{cfg: cfg, Store: st, Git: gitx.Git{Bin: cfg.GitBin}, ops: map[string]Op{}}, nil
}

// Close closes the database.
func (a *App) Close() error { return a.Store.Close() }

// DataDir is the resolved data directory.
func (a *App) DataDir() string { return a.cfg.DataDir }

// Error is a domain error that maps onto an HTTP status.
type Error struct {
	Code int
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func errf(code int, format string, args ...any) error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

func isConflict(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == http.StatusConflict
}

func notFound(what string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return errf(http.StatusNotFound, "%s not found", what)
	}
	return err
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

func validName(kind, s string) error {
	if !nameRe.MatchString(s) {
		return errf(http.StatusBadRequest, "invalid %s %q: use lowercase letters, digits, '.', '_' and '-'", kind, s)
	}
	return nil
}

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func nextNumber(ctx context.Context, q querier, table, scopeCol, scope string) (int, error) {
	var n sql.NullInt64
	err := q.QueryRowContext(ctx, fmt.Sprintf(`SELECT MAX(number) FROM %s WHERE %s = ?`, table, scopeCol), scope).Scan(&n)
	return int(n.Int64) + 1, err
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
