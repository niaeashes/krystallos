// Package ident parses and formats identity strings.
//
// An identity is written <kind>:<root>(/<label>)*[@<host>], for example
// user:alice, user:alice/claude/explorer or bot:devbox@krystallos.example.com.
// The kind and root name the resource that holds the identity (a user, a
// bot or a service); each label derives a new identity from the one before.
package ident

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Kind is the kind of resource an identity belongs to.
type Kind string

const (
	User    Kind = "user"
	Bot     Kind = "bot"
	Service Kind = "service"
)

// Unattributed marks a change that reached a repository without passing
// through the server. It is not an identity.
const Unattributed = "unattributed"

var segment = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

var hostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?$`)

// MaxLen bounds an identity's short form, so a deep derivation stays a
// reasonable key.
const MaxLen = 255

// ID is a parsed identity.
type ID struct {
	Kind   Kind
	Name   string   // the root name
	Labels []string // derived labels below the root, outermost last
	Host   string   // empty for the local server
}

// ValidSegment reports whether s can be a root name or a label.
func ValidSegment(s string) bool { return segment.MatchString(s) }

// ValidHost reports whether s can be a server host.
func ValidHost(s string) bool { return hostRe.MatchString(s) }

// Parse reads an identity string.
func Parse(s string) (ID, error) {
	kind, rest, ok := strings.Cut(s, ":")
	if !ok {
		return ID{}, fmt.Errorf("identity %q has no kind (user:, bot: or service:)", s)
	}
	id := ID{Kind: Kind(kind)}
	switch id.Kind {
	case User, Bot, Service:
	default:
		return ID{}, fmt.Errorf("identity %q has an unknown kind %q", s, kind)
	}
	path, host, hasHost := strings.Cut(rest, "@")
	if hasHost {
		if !ValidHost(host) {
			return ID{}, fmt.Errorf("identity %q has an invalid host", s)
		}
		id.Host = host
	}
	parts := strings.Split(path, "/")
	for _, p := range parts {
		if !ValidSegment(p) {
			return ID{}, fmt.Errorf("identity %q has an invalid part %q", s, p)
		}
	}
	id.Name, id.Labels = parts[0], parts[1:]
	if len(id.String()) > MaxLen {
		return ID{}, errors.New("identity is too long")
	}
	return id, nil
}

// MustParse parses s or panics; for constants.
func MustParse(s string) ID {
	id, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return id
}

// Root returns the identity at the root of the derivation tree.
func Root(kind Kind, name string) ID { return ID{Kind: kind, Name: name} }

// String is the short form, without the host.
func (id ID) String() string {
	var b strings.Builder
	b.WriteString(string(id.Kind))
	b.WriteByte(':')
	b.WriteString(id.Name)
	for _, l := range id.Labels {
		b.WriteByte('/')
		b.WriteString(l)
	}
	return b.String()
}

// Full is the identity qualified with a host.
func (id ID) Full(host string) string {
	if id.Host != "" {
		host = id.Host
	}
	return id.String() + "@" + host
}

// IsRoot reports whether id has no labels.
func (id ID) IsRoot() bool { return len(id.Labels) == 0 }

// RootID drops the labels.
func (id ID) RootID() ID { return ID{Kind: id.Kind, Name: id.Name, Host: id.Host} }

// Parent returns the identity id was derived from.
func (id ID) Parent() (ID, bool) {
	if id.IsRoot() {
		return ID{}, false
	}
	return ID{Kind: id.Kind, Name: id.Name, Labels: id.Labels[:len(id.Labels)-1], Host: id.Host}, true
}

// Child derives a new identity below id.
func (id ID) Child(label string) (ID, error) {
	if !ValidSegment(label) {
		return ID{}, fmt.Errorf("invalid label %q", label)
	}
	labels := append(append([]string(nil), id.Labels...), label)
	child := ID{Kind: id.Kind, Name: id.Name, Labels: labels, Host: id.Host}
	if len(child.String()) > MaxLen {
		return ID{}, errors.New("identity is too long")
	}
	return child, nil
}

// Chain lists id and its ancestors, from id up to the root.
func (id ID) Chain() []ID {
	out := []ID{id}
	for cur := id; !cur.IsRoot(); {
		cur, _ = cur.Parent()
		out = append(out, cur)
	}
	return out
}

// Within reports whether id is other or derived from it.
func (id ID) Within(other ID) bool {
	if id.Kind != other.Kind || id.Name != other.Name || len(id.Labels) < len(other.Labels) {
		return false
	}
	for i, l := range other.Labels {
		if id.Labels[i] != l {
			return false
		}
	}
	return true
}

// Below reports whether id is strictly derived from other.
func (id ID) Below(other ID) bool { return id.Within(other) && len(id.Labels) > len(other.Labels) }
