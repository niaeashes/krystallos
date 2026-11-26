package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

type mcpClient struct {
	t  *testing.T
	cs *mcp.ClientSession
}

func connectMCP(t *testing.T, endpoint, token string) mcpClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "krystallos-test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: bearer{token: token, base: http.DefaultTransport}},
		MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return mcpClient{t: t, cs: cs}
}

// call runs a tool and decodes its structured result into a map. With
// wantErr it expects the tool to fail and returns the message.
func (c mcpClient) call(name string, args map[string]any, wantErr bool) map[string]any {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	res, err := c.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		c.t.Fatalf("%s: %v", name, err)
	}
	text := ""
	for _, ct := range res.Content {
		if tc, ok := ct.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	if res.IsError != wantErr {
		c.t.Fatalf("%s: IsError = %v, want %v: %s", name, res.IsError, wantErr, text)
	}
	if wantErr {
		return map[string]any{"error": text}
	}
	raw, _ := json.Marshal(res.StructuredContent)
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		c.t.Fatalf("%s: decode %s: %v", name, raw, err)
	}
	return out
}

// TestMCP drives the MCP endpoint with the official client, as an agent's
// identity derived from a user: with its standing token it opens a session,
// and through the session it creates a repository, pushes to it, plans and
// opens a change set, opens a child session, and reads the event stream.
func TestMCP(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(t, dir)
	adminTok := tokenFrom(t, h.run("admin", "init", "--data", h.data, "--user", "niaeashes"))
	h.start()
	admin := h.client(adminTok)
	ok[map[string]any](t, admin, "POST", "/api/orgs", map[string]string{"name": "niaeashes"})
	ok[map[string]string](t, admin, "POST", "/api/identities", map[string]string{"label": "claude"})
	claudeTok := ok[map[string]any](t, admin, "POST", "/api/tokens", map[string]any{"identity": "user:niaeashes/claude"})["token"].(string)

	// Without a token there is no MCP.
	if code, _ := (client{h: h}).call("POST", "/mcp", map[string]any{}); code != http.StatusUnauthorized {
		t.Fatalf("MCP without a token should be refused, got %d", code)
	}

	// The endpoint is stateless, so a plain JSON-RPC request can call a tool
	// without a handshake, as `curl` would.
	req, _ := http.NewRequest("POST", h.base+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`))
	req.Header.Set("Authorization", "Bearer "+claudeTok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "user:niaeashes/claude") {
		t.Fatalf("raw tools/call: %d %s", resp.StatusCode, raw)
	}

	standing := connectMCP(t, h.base+"/mcp", claudeTok)
	tools, err := standing.cs.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) < 20 {
		t.Fatalf("expected the full tool set: %v %v", err, tools)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
	}
	// An agent opens sessions; it does not make identities or standing tokens.
	if !names["open_session"] || names["derive_identity"] || names["issue_token"] {
		t.Fatalf("expected open_session, and neither derive_identity nor issue_token: %v", names)
	}
	for _, gone := range []string{"derive_identity", "issue_token"} {
		res, err := standing.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: gone, Arguments: map[string]any{"label": "x", "identity": "user:niaeashes/claude"}})
		if err == nil && !res.IsError {
			t.Fatalf("%s should be gone: %v", gone, res)
		}
	}
	if me := standing.call("whoami", map[string]any{}, false); me["identity"] != "user:niaeashes/claude" || me["session"] != "" {
		t.Fatalf("a standing token belongs to no session: %v", me)
	}

	// The agent's run opens a session and works through it.
	opened := standing.call("open_session", map[string]any{"label": "sess-mcp"}, false)
	session, _ := opened["session"].(map[string]any)
	sessID, _ := session["id"].(string)
	sessTok, _ := opened["token"].(string)
	if sessID == "" || sessTok == "" || session["parent"] != "" || session["label"] != "sess-mcp" || opened["committer_email"] != "user:niaeashes/claude@localhost" {
		t.Fatalf("unexpected open_session: %v", opened)
	}
	c := connectMCP(t, h.base+"/mcp", sessTok)
	me := c.call("whoami", map[string]any{}, false)
	if me["identity"] != "user:niaeashes/claude" || me["session"] != sessID || me["commit_email"] != "user:niaeashes/claude@localhost" {
		t.Fatalf("unexpected whoami: %v", me)
	}

	// The agent inherits its user's org admin, so it can create a repository.
	created := c.call("create_repo", map[string]any{"org": "niaeashes", "name": "demo"}, false)
	if rule := created["repo"].(map[string]any)["merge_rule"].(map[string]any); rule["required_checks"] == nil {
		t.Fatalf("a new repository's merge rule should list no checks, not null: %v", rule)
	}
	if created["clone_url"] != h.url+"/git/niaeashes/demo.git" {
		t.Fatalf("the clone URL should be under the server's URL %s: %v", h.url, created)
	}

	// Push with the session's token, committing as the agent.
	work := filepath.Join(dir, "demo")
	h.git(dir, "clone", h.repoURL(sessTok, "niaeashes/demo"), work)
	h.commitAs(work, "claude", "user:niaeashes/claude@localhost", "README.md", "# demo\n", "start demo")
	h.git(work, "push", "origin", "HEAD:main")
	h.git(work, "checkout", "-q", "-b", "feature")
	h.commitAs(work, "claude", "user:niaeashes/claude@localhost", "notes.md", "notes\n", "add notes")
	h.git(work, "push", "origin", "feature")

	issue := c.call("create_issue", map[string]any{"repo": "niaeashes/demo", "title": "write notes"}, false)
	plan := c.call("create_plan", map[string]any{"repo": "niaeashes/demo", "issue": issue["number"], "title": "one file", "steps": []string{"add notes.md"}}, false)
	cs := c.call("create_changeset", map[string]any{
		"repo": "niaeashes/demo", "issue": issue["number"], "title": "notes", "branch": "feature",
		"plan": plan["number"], "steps": []string{"s1"},
	}, false)
	view := c.call("get_changeset", map[string]any{"repo": "niaeashes/demo", "number": cs["number"]}, false)
	if view["author"] != "user:niaeashes/claude" {
		t.Fatalf("the change set should be the agent's: %v", view)
	}
	diff := c.call("get_changeset_diff", map[string]any{"repo": "niaeashes/demo", "number": cs["number"]}, false)
	if files, _ := diff["files"].([]any); len(files) != 1 {
		t.Fatalf("unexpected diff: %v", diff)
	}
	file := c.call("read_file", map[string]any{"repo": "niaeashes/demo", "ref": "feature", "path": "notes.md"}, false)
	if f, _ := file["file"].(map[string]any); f["content"] != "notes\n" {
		t.Fatalf("unexpected read_file: %v", file)
	}
	commits := c.call("list_commits", map[string]any{"repo": "niaeashes/demo", "ref": "feature"}, false)
	if list, _ := commits["commits"].([]any); len(list) != 2 || list[0].(map[string]any)["committer_identity"] != "user:niaeashes/claude" {
		t.Fatalf("unexpected list_commits: %v", commits)
	}

	// A subagent's run is a child session of the run, under the same
	// identity, and its token works over MCP.
	sub := c.call("open_session", map[string]any{"label": "explorer", "ttl_hours": 1}, false)
	subSession, _ := sub["session"].(map[string]any)
	if subSession["parent"] != sessID || subSession["identity"] != "user:niaeashes/claude" {
		t.Fatalf("unexpected child session: %v", sub)
	}
	if subMe := connectMCP(t, h.base+"/mcp", sub["token"].(string)).call("whoami", map[string]any{}, false); subMe["session"] != subSession["id"] {
		t.Fatalf("the child session's token should carry it: %v", subMe)
	}
	c.call("open_session", map[string]any{"ttl_hours": 25}, true)

	// Errors come back as tool errors, not as protocol failures.
	if res := c.call("get_issue", map[string]any{"repo": "niaeashes/nope", "number": 1}, true); !strings.Contains(res["error"].(string), "not found") {
		t.Fatalf("unexpected error text: %v", res)
	}

	// The event stream is readable over MCP, and every action above is in it
	// under the agent's identity and session.
	evs := c.call("wait_for_events", map[string]any{"after": 0}, false)
	list, _ := evs["events"].([]any)
	var sawRepo, sawChangeSet, sawChild bool
	for _, e := range list {
		m := e.(map[string]any)
		if m["identity"] == "user:niaeashes/claude" && m["session"] == sessID {
			sawRepo = sawRepo || m["type"] == "repo.created"
			sawChangeSet = sawChangeSet || m["type"] == "changeset.created"
			p, _ := m["payload"].(map[string]any)
			sawChild = sawChild || (m["type"] == "session.opened" && p["id"] == subSession["id"] && p["parent"] == sessID)
		}
	}
	if !sawRepo || !sawChangeSet || !sawChild {
		t.Fatalf("expected repo.created, changeset.created and the child's session.opened by the agent's session in %d events", len(list))
	}
	next := evs["next"].(float64)
	start := time.Now()
	waited := c.call("wait_for_events", map[string]any{"after": next, "wait_seconds": 1}, false)
	if got, _ := waited["events"].([]any); len(got) != 0 || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("waiting past the end should return nothing after the wait: %v", waited)
	}
}
