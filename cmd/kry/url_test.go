package main

import (
	"slices"
	"testing"
)

func TestResolveAddress(t *testing.T) {
	for _, tc := range []struct {
		name      string
		url       string
		origin    string
		originSet bool
		host      string
		port      string
		want      string
		origins   []string
	}{
		{name: "a real host is served over https", host: "git.example.com", port: "3000",
			want: "https://git.example.com"},
		{name: "a host with a port keeps it", host: "git.example.com:8443", port: "3000",
			want: "https://git.example.com:8443"},
		{name: "localhost is served where it listens, and Vite is accepted", host: "localhost", port: "4000",
			want: "http://localhost:4000", origins: []string{"http://localhost:5173"}},
		{name: "a localhost host with a port uses that port", host: "localhost:8080", port: "3000",
			want: "http://localhost:8080", origins: []string{"http://localhost:5173"}},
		{name: "--origin replaces the Vite default", host: "localhost", port: "3000",
			origin: "http://localhost:4173, http://LOCALHOST:4174/", originSet: true,
			want: "http://localhost:3000", origins: []string{"http://localhost:4173", "http://localhost:4174"}},
		{name: "an empty --origin accepts only the URL", host: "localhost", port: "3000", originSet: true,
			want: "http://localhost:3000"},
		{name: "--url wins over the host", url: "https://Git.Example.com/", host: "localhost", port: "3000",
			want: "https://git.example.com"},
		{name: "--url at localhost on another port still accepts Vite", url: "http://localhost:8080", host: "localhost", port: "3000",
			want: "http://localhost:8080", origins: []string{"http://localhost:5173"}},
		{name: "a default port is dropped", url: "https://git.example.com:443", host: "x", port: "3000",
			want: "https://git.example.com"},
		{name: "the URL is not repeated as an extra origin", url: "http://localhost:5173", host: "localhost", port: "3000",
			want: "http://localhost:5173"},
		{name: "extra origins with a real host", host: "git.example.com", port: "3000",
			origin: "https://www.example.com", originSet: true,
			want: "https://git.example.com", origins: []string{"https://www.example.com"}},
		{name: "IPv6", url: "http://[::1]:3000", host: "localhost", port: "3000",
			want: "http://[::1]:3000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveAddress(tc.url, tc.origin, tc.originSet, tc.host, tc.port)
			if err != nil {
				t.Fatal(err)
			}
			if got.URL != tc.want || !slices.Equal(got.Origins, tc.origins) {
				t.Errorf("got %q %q, want %q %q", got.URL, got.Origins, tc.want, tc.origins)
			}
		})
	}
}

func TestResolveAddressRefuses(t *testing.T) {
	for _, tc := range []struct{ name, url, origin string }{
		{"a path", "https://git.example.com/krystallos", ""},
		{"a query", "https://git.example.com?x=1", ""},
		{"an empty query", "https://git.example.com?", ""},
		{"a fragment", "https://git.example.com#top", ""},
		{"no scheme", "git.example.com", ""},
		{"host and port without a scheme", "localhost:3000", ""},
		{"another scheme", "ssh://git.example.com", ""},
		{"no host", "https://", ""},
		{"a user", "https://alice@git.example.com", ""},
		{"a bad port", "https://git.example.com:https", ""},
		{"a bad extra origin", "", "localhost:5173"},
		{"an extra origin with a path", "", "http://localhost:5173/app"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := resolveAddress(tc.url, tc.origin, tc.origin != "", "localhost", "3000"); err == nil {
				t.Errorf("accepted --url %q --origin %q as %v", tc.url, tc.origin, got)
			}
		})
	}
}

func TestPublicURLForAdmin(t *testing.T) {
	// admin does not know where serve listens, so a localhost server is
	// taken to be on the default port.
	for host, want := range map[string]string{
		"localhost":       "http://localhost:3000",
		"git.example.com": "https://git.example.com",
	} {
		if got, err := publicURL("", host, "3000"); err != nil || got != want {
			t.Errorf("host %s: got %q %v, want %q", host, got, err, want)
		}
	}
}

func TestPasskeyProblem(t *testing.T) {
	for url, ok := range map[string]bool{
		"https://git.example.com":     true,
		"http://localhost:3000":       true,
		"http://krystallos.localhost": true,
		"http://git.example.com":      false,
		"http://127.0.0.1:3000":       false,
		"https://192.0.2.1":           false,
		"http://[::1]:3000":           false,
		"https://git.example.com:84":  true,
	} {
		if got := passkeyProblem(url); (got == "") != ok {
			t.Errorf("%s: problem %q, want ok=%v", url, got, ok)
		}
	}
}
