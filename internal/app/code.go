package app

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Reading code always resolves to a commit first, so a read can be recorded
// with the exact version that was seen.

// ResolveCommit turns a branch, tag or object id into a commit id. An empty
// rev means the default branch.
func (a *App) ResolveCommit(ctx context.Context, repo Repo, rev string) (string, error) {
	if rev == "" {
		rev = repo.DefaultBranch
	}
	if strings.HasPrefix(rev, "-") || strings.ContainsAny(rev, " \t\n:") {
		return "", errf(http.StatusBadRequest, "invalid revision %q", rev)
	}
	oid, err := a.Git.Output(ctx, repo.Path, nil, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil || oid == "" {
		return "", errf(http.StatusNotFound, "revision %q not found in %s", rev, repo.FullName)
	}
	return oid, nil
}

func cleanPath(p string) (string, error) {
	p = strings.Trim(p, "/")
	for _, part := range strings.Split(p, "/") {
		if part == ".." || part == "." {
			return "", errf(http.StatusBadRequest, "invalid path %q", p)
		}
	}
	return p, nil
}

// Ref is a branch or tag.
type Ref struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Commit string `json:"commit"`
}

// Refs lists branches and tags.
func (a *App) Refs(ctx context.Context, repo Repo) ([]Ref, error) {
	out, err := a.Git.Output(ctx, repo.Path, nil, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(*objectname)", "refs/heads", "refs/tags")
	if err != nil {
		return nil, err
	}
	refs := []Ref{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\x00")
		if len(f) < 3 {
			continue
		}
		r := Ref{Commit: f[1]}
		if f[2] != "" {
			r.Commit = f[2] // annotated tag: point at the commit
		}
		switch {
		case strings.HasPrefix(f[0], "refs/heads/"):
			r.Name, r.Kind = strings.TrimPrefix(f[0], "refs/heads/"), "branch"
		case strings.HasPrefix(f[0], "refs/tags/"):
			r.Name, r.Kind = strings.TrimPrefix(f[0], "refs/tags/"), "tag"
		}
		refs = append(refs, r)
	}
	return refs, nil
}

// TreeEntry is one entry of a directory listing.
type TreeEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"`
	Mode string `json:"mode"`
	OID  string `json:"oid"`
	Size int64  `json:"size,omitempty"`
}

// Tree lists a directory at a commit.
func (a *App) Tree(ctx context.Context, repo Repo, commit, path string) ([]TreeEntry, error) {
	path, err := cleanPath(path)
	if err != nil {
		return nil, err
	}
	out, err := a.Git.Output(ctx, repo.Path, nil, "ls-tree", "-l", "-z", commit+":"+path)
	if err != nil {
		return nil, errf(http.StatusNotFound, "directory %q not found at %s", path, short(commit))
	}
	entries := []TreeEntry{}
	for _, rec := range strings.Split(out, "\x00") {
		meta, name, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta)
		if len(f) < 4 {
			continue
		}
		e := TreeEntry{Name: name, Mode: f[0], Type: f[1], OID: f[2]}
		if path == "" {
			e.Path = name
		} else {
			e.Path = path + "/" + name
		}
		if f[3] != "-" {
			e.Size, _ = strconv.ParseInt(f[3], 10, 64)
		}
		entries = append(entries, e)
	}
	// Directories first, then files, each alphabetically (git already sorts
	// by name).
	sorted := make([]TreeEntry, 0, len(entries))
	for _, e := range entries {
		if e.Type == "tree" {
			sorted = append(sorted, e)
		}
	}
	for _, e := range entries {
		if e.Type != "tree" {
			sorted = append(sorted, e)
		}
	}
	return sorted, nil
}

// Blob is a file's content at a commit.
type Blob struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Binary    bool   `json:"binary"`
	Truncated bool   `json:"truncated"`
	Content   string `json:"content"`
}

const maxBlob = 1 << 20

// Blob reads a file at a commit.
func (a *App) Blob(ctx context.Context, repo Repo, commit, path string) (Blob, error) {
	path, err := cleanPath(path)
	if err != nil {
		return Blob{}, err
	}
	b := Blob{Path: path}
	size, err := a.Git.Output(ctx, repo.Path, nil, "cat-file", "-s", commit+":"+path)
	if err != nil {
		return Blob{}, errf(http.StatusNotFound, "file %q not found at %s", path, short(commit))
	}
	b.Size, _ = strconv.ParseInt(size, 10, 64)
	cmd := a.Git.Cmd(ctx, repo.Path, nil, "cat-file", "blob", commit+":"+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Blob{}, err
	}
	if err := cmd.Start(); err != nil {
		return Blob{}, err
	}
	data, _ := io.ReadAll(io.LimitReader(stdout, maxBlob+1))
	io.Copy(io.Discard, stdout)
	cmd.Wait()
	if len(data) > maxBlob {
		data, b.Truncated = data[:maxBlob], true
	}
	sniff := data
	if len(sniff) > 8000 {
		sniff = sniff[:8000]
	}
	if bytes.IndexByte(sniff, 0) >= 0 {
		b.Binary = true
		return b, nil
	}
	b.Content = string(data)
	return b, nil
}

// Commit is one commit's metadata.
type Commit struct {
	OID            string   `json:"oid"`
	Parents        []string `json:"parents"`
	AuthorName     string   `json:"author_name"`
	AuthorEmail    string   `json:"author_email"`
	AuthorDate     string   `json:"author_date"`
	CommitterName  string   `json:"committer_name"`
	CommitterEmail string   `json:"committer_email"`
	CommitterDate  string   `json:"committer_date"`
	Subject        string   `json:"subject"`
	Body           string   `json:"body"`
	// The identities the addresses resolve to, if any. The committer's is
	// the commit's identity; the author's is a claim.
	CommitterIdentity string `json:"committer_identity,omitempty"`
	AuthorIdentity    string `json:"author_identity,omitempty"`
}

// Log lists commits reachable from a commit, newest first, optionally only
// those touching a path. No commit is listed after one of its parents, even
// when they share a commit time: plain git log orders by time alone and, on
// a tie, can put a parent ahead of its children; --date-order keeps the
// graph's order and uses time only where the graph leaves a choice.
func (a *App) Log(ctx context.Context, repo Repo, commit, path string, limit int) ([]Commit, error) {
	path, err := cleanPath(path)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	args := []string{"log", "-z", "--date-order", "-n", strconv.Itoa(limit),
		"--format=%H%x1f%P%x1f%an%x1f%ae%x1f%aI%x1f%cn%x1f%ce%x1f%cI%x1f%s%x1f%b", commit}
	if path != "" {
		args = append(args, "--", path)
	}
	out, err := a.Git.Output(ctx, repo.Path, nil, args...)
	if err != nil {
		return nil, err
	}
	commits := []Commit{}
	for _, rec := range strings.Split(out, "\x00") {
		f := strings.Split(strings.TrimLeft(rec, "\n"), "\x1f")
		if len(f) < 10 {
			continue
		}
		commits = append(commits, Commit{
			OID: f[0], Parents: strings.Fields(f[1]),
			AuthorName: f[2], AuthorEmail: f[3], AuthorDate: f[4],
			CommitterName: f[5], CommitterEmail: f[6], CommitterDate: f[7],
			Subject: f[8], Body: strings.TrimSpace(f[9]),
		})
	}
	resolved := map[string]string{}
	resolve := func(email string) (string, error) {
		if id, ok := resolved[email]; ok {
			return id, nil
		}
		id, _, err := a.ResolveEmail(ctx, a.Store.DB(), email)
		resolved[email] = id
		return id, err
	}
	for i := range commits {
		c := &commits[i]
		if c.CommitterIdentity, err = resolve(c.CommitterEmail); err != nil {
			return nil, err
		}
		if c.AuthorIdentity, err = resolve(c.AuthorEmail); err != nil {
			return nil, err
		}
	}
	return commits, nil
}

// FileDiff is one file's part of a diff.
type FileDiff struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Binary    bool   `json:"binary"`
	Truncated bool   `json:"truncated"`
	Patch     string `json:"patch"`
}

// Diff is what a change set changes, against the point it branched from.
type Diff struct {
	Base      string     `json:"base"`
	Head      string     `json:"head"`
	MergeBase string     `json:"merge_base"`
	Files     []FileDiff `json:"files"`
}

const maxFilePatch = 256 << 10

// ChangeSetDiff diffs a change set's head against its merge base with the
// base branch. For a merged change set it uses the two parents of the merge
// commit, so the diff stays what was reviewed.
func (a *App) ChangeSetDiff(ctx context.Context, fullName string, number int) (Diff, error) {
	db := a.Store.DB()
	repo, err := a.Repo(ctx, db, fullName)
	if err != nil {
		return Diff{}, err
	}
	cs, err := a.changeSet(ctx, db, fullName, number)
	if err != nil {
		return Diff{}, err
	}
	var d Diff
	if cs.MergedCommit != "" {
		if d.Base, err = a.ResolveCommit(ctx, repo, cs.MergedCommit+"^1"); err != nil {
			return Diff{}, err
		}
		if d.Head, err = a.ResolveCommit(ctx, repo, cs.MergedCommit+"^2"); err != nil {
			return Diff{}, err
		}
	} else {
		if d.Base, err = a.resolveBranch(ctx, repo, cs.Base); err != nil {
			return Diff{}, err
		}
		if d.Head, err = a.resolveBranch(ctx, repo, cs.Branch); err != nil {
			return Diff{}, err
		}
	}
	if d.MergeBase, err = a.Git.Output(ctx, repo.Path, nil, "merge-base", d.Base, d.Head); err != nil {
		return Diff{}, errf(http.StatusConflict, "%s and %s share no history", cs.Base, cs.Branch)
	}
	d.Files, err = a.diffFiles(ctx, repo, d.MergeBase, d.Head)
	return d, err
}

func (a *App) diffFiles(ctx context.Context, repo Repo, from, to string) ([]FileDiff, error) {
	common := []string{"-M", "--no-color", "--no-ext-diff"}
	status, err := a.Git.Output(ctx, repo.Path, nil, append([]string{"diff", "-z", "--name-status"}, append(common, from, to)...)...)
	if err != nil {
		return nil, err
	}
	numstat, err := a.Git.Output(ctx, repo.Path, nil, append([]string{"diff", "-z", "--numstat"}, append(common, from, to)...)...)
	if err != nil {
		return nil, err
	}
	patch, err := a.Git.Output(ctx, repo.Path, nil, append([]string{"diff"}, append(common, from, to)...)...)
	if err != nil {
		return nil, err
	}

	// --name-status -z: STATUS \0 path \0, or for renames and copies
	// STATUS \0 old \0 new \0.
	files := []FileDiff{}
	tok := strings.Split(status, "\x00")
	for i := 0; i < len(tok); i++ {
		st := tok[i]
		if st == "" {
			continue
		}
		f := FileDiff{Status: st[:1]}
		if (f.Status == "R" || f.Status == "C") && i+2 < len(tok) {
			f.OldPath, f.Path = tok[i+1], tok[i+2]
			i += 2
		} else if i+1 < len(tok) {
			f.Path = tok[i+1]
			i++
		}
		files = append(files, f)
	}

	// --numstat -z: "add \t del \t path \0", or for renames
	// "add \t del \t \0 old \0 new \0". Binary files show "-".
	tok = strings.Split(numstat, "\x00")
	n := 0
	for i := 0; i < len(tok) && n < len(files); i++ {
		parts := strings.SplitN(tok[i], "\t", 3)
		if len(parts) < 3 {
			continue
		}
		if parts[0] == "-" {
			files[n].Binary = true
		} else {
			files[n].Additions, _ = strconv.Atoi(parts[0])
			files[n].Deletions, _ = strconv.Atoi(parts[1])
		}
		if parts[2] == "" {
			i += 2 // the rename's old and new paths follow
		}
		n++
	}

	// The full patch, split at each file header. Files come in the same
	// order as above.
	chunks := splitPatch(patch)
	for i := range files {
		if i < len(chunks) {
			p := chunks[i]
			if len(p) > maxFilePatch {
				p, files[i].Truncated = p[:maxFilePatch], true
			}
			files[i].Patch = p
		}
	}
	return files, nil
}

func splitPatch(patch string) []string {
	if patch == "" {
		return nil
	}
	var chunks []string
	start := 0
	for {
		next := strings.Index(patch[start+1:], "\ndiff --git ")
		if next < 0 {
			chunks = append(chunks, patch[start:])
			return chunks
		}
		end := start + 1 + next + 1
		chunks = append(chunks, strings.TrimSuffix(patch[start:end], "\n"))
		start = end
	}
}
