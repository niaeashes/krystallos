package ident

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	id, err := Parse("user:alice/claude/explorer@gitsynth.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if id.Kind != User || id.Name != "alice" || strings.Join(id.Labels, "/") != "claude/explorer" || id.Host != "gitsynth.example.com" {
		t.Fatalf("unexpected parse: %+v", id)
	}
	if id.String() != "user:alice/claude/explorer" {
		t.Fatalf("short form = %q", id.String())
	}
	if id.Full("other") != "user:alice/claude/explorer@gitsynth.example.com" {
		t.Fatalf("full form keeps its own host: %q", id.Full("other"))
	}
	for _, bad := range []string{"alice", "robot:x", "user:", "user:Alice", "user:alice//x", "user:alice/-x", "user:alice@", "user:alice@bad host"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}

func TestDerivation(t *testing.T) {
	root := MustParse("bot:devbox")
	child, err := root.Child("triage")
	if err != nil {
		t.Fatal(err)
	}
	grand, _ := child.Child("haiku")
	if grand.String() != "bot:devbox/triage/haiku" {
		t.Fatalf("grandchild = %q", grand)
	}
	chain := grand.Chain()
	if len(chain) != 3 || chain[0].String() != grand.String() || chain[2].String() != "bot:devbox" {
		t.Fatalf("chain = %v", chain)
	}
	if !grand.Within(root) || !grand.Below(child) || child.Below(grand) || root.Below(root) || !root.Within(root) {
		t.Fatal("ancestry checks are wrong")
	}
	if MustParse("bot:devboxx").Within(root) || MustParse("user:devbox").Within(root) {
		t.Fatal("a different root must not count as within")
	}
	if p, ok := child.Parent(); !ok || p.String() != "bot:devbox" {
		t.Fatalf("parent = %v %v", p, ok)
	}
	if _, ok := root.Parent(); ok {
		t.Fatal("a root has no parent")
	}
}
