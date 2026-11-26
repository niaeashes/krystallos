// Package gitx runs the git binary and reads the pkt-line format git speaks
// over the wire.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Git runs a git binary.
type Git struct {
	Bin string
}

// Cmd prepares a git command in dir with extra environment variables.
//
// git runs without the server user's git configuration: the repositories
// are krystallos's, not that user's. A core.hooksPath in the global or system
// config would make git skip each repository's reference-transaction hook,
// a "* -diff" in the user's or the system's gitattributes would turn every
// change set diff into "Binary files differ" and hide the change from
// review, and GIT_* variables in the server's environment (GIT_DIR,
// GIT_CONFIG_PARAMETERS, GIT_CONFIG_COUNT and the like) could redirect or
// reconfigure git the same way. So every GIT_* variable is dropped from the
// inherited environment, GIT_EXEC_PATH included (a git packaged to need it
// has to find its helpers another way); the system and global config files
// and the system and user attributes files are not read. env is added after
// that, so a caller can still set GIT_* on purpose (GIT_PROTOCOL, a merge's
// author and committer). A caller that sets GIT_CONFIG_COUNT replaces the
// entry that switches off the user's attributes file; none does. PATH, HOME
// and the rest are kept, and hooks inherit the result.
//
// With global and system config ignored, safe.directory cannot excuse a
// repository owned by someone else: the server has to run as the user that
// owns the data directory.
func (g Git) Cmd(ctx context.Context, dir string, env []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, g.Bin, args...)
	cmd.Dir = dir
	cmd.Env = environ(os.Environ(), env)
	return cmd
}

// environ is base without any GIT_* variable, with the system and global
// config and the system and user attributes switched off, followed by env.
func environ(base, env []string) []string {
	isolate := []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_ATTR_NOSYSTEM=1",
		// core.attributesFile defaults to $XDG_CONFIG_HOME/git/attributes
		// or ~/.config/git/attributes, with no global config to change it.
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.attributesFile",
		"GIT_CONFIG_VALUE_0=" + os.DevNull,
	}
	out := make([]string, 0, len(base)+len(isolate)+len(env))
	for _, kv := range base {
		if !strings.HasPrefix(kv, "GIT_") {
			out = append(out, kv)
		}
	}
	out = append(out, isolate...)
	return append(out, env...)
}

// Output runs git and returns its trimmed stdout. A failure carries stderr.
func (g Git) Output(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := g.Cmd(ctx, dir, env, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(stdout.String()), &Error{Args: args, Err: err, Stderr: strings.TrimSpace(stderr.String())}
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Error is a failed git invocation.
type Error struct {
	Args   []string
	Err    error
	Stderr string
}

func (e *Error) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args, " "), e.Err, e.Stderr)
}

// ExitCode returns the exit code of a failed git invocation, or -1.
func ExitCode(err error) int {
	var ge *Error
	if errors.As(err, &ge) {
		var ee *exec.ExitError
		if errors.As(ge.Err, &ee) {
			return ee.ExitCode()
		}
	}
	return -1
}

// IsOID reports whether s looks like a full object id (SHA-1 or SHA-256).
func IsOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// IsZeroOID reports whether oid is the all-zero id git uses for "no object".
func IsZeroOID(oid string) bool {
	return IsOID(oid) && strings.Trim(oid, "0") == ""
}

// Pkt is one pkt-line. Special is "flush", "delim" or "response-end" for the
// marker packets, and empty for data packets.
type Pkt struct {
	Special string
	Data    []byte
}

// ReadPkts splits b into pkt-lines.
func ReadPkts(b []byte) ([]Pkt, error) {
	var out []Pkt
	for len(b) > 0 {
		if len(b) < 4 {
			return out, errors.New("short pkt-line")
		}
		n, err := strconv.ParseUint(string(b[:4]), 16, 16)
		if err != nil {
			return out, fmt.Errorf("bad pkt-line length %q", b[:4])
		}
		switch n {
		case 0:
			out = append(out, Pkt{Special: "flush"})
			b = b[4:]
			continue
		case 1:
			out = append(out, Pkt{Special: "delim"})
			b = b[4:]
			continue
		case 2:
			out = append(out, Pkt{Special: "response-end"})
			b = b[4:]
			continue
		}
		if n < 4 || int(n) > len(b) {
			return out, fmt.Errorf("bad pkt-line length %d", n)
		}
		out = append(out, Pkt{Data: b[4:n]})
		b = b[n:]
	}
	return out, nil
}

// PktLine encodes s as one pkt-line.
func PktLine(s string) []byte {
	return []byte(fmt.Sprintf("%04x%s", len(s)+4, s))
}

// UploadRequest is what a protocol v2 upload-pack request asked for.
type UploadRequest struct {
	Command     string   `json:"command"`
	Wants       []string `json:"wants,omitempty"`
	WantRefs    []string `json:"want_refs,omitempty"`
	RefPrefixes []string `json:"ref_prefixes,omitempty"`
	Haves       int      `json:"haves,omitempty"`
	Done        bool     `json:"done,omitempty"`
}

// ParseUploadRequest reads a protocol v2 upload-pack request body.
func ParseUploadRequest(b []byte) (UploadRequest, error) {
	pkts, err := ReadPkts(b)
	var req UploadRequest
	for _, p := range pkts {
		if p.Special != "" {
			continue
		}
		line := strings.TrimSuffix(string(p.Data), "\n")
		switch {
		case strings.HasPrefix(line, "command="):
			req.Command = strings.TrimPrefix(line, "command=")
		case strings.HasPrefix(line, "want-ref "):
			req.WantRefs = append(req.WantRefs, strings.TrimPrefix(line, "want-ref "))
		case strings.HasPrefix(line, "want "):
			if f := strings.Fields(line); len(f) >= 2 {
				req.Wants = append(req.Wants, f[1])
			}
		case strings.HasPrefix(line, "have "):
			req.Haves++
		case strings.HasPrefix(line, "ref-prefix "):
			req.RefPrefixes = append(req.RefPrefixes, strings.TrimPrefix(line, "ref-prefix "))
		case line == "done":
			req.Done = true
		}
	}
	return req, err
}
