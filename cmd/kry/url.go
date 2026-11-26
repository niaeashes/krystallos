package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
)

// devOrigin is the Vite dev server (npm --prefix web run dev). A server at
// localhost accepts passkeys from it too, unless --origin says otherwise.
const devOrigin = "http://localhost:5173"

// address is where the server is reached.
type address struct {
	// URL is where people open the UI and where git and MCP clients
	// connect, as scheme://host[:port]. Its host name is the WebAuthn
	// relying party ID.
	URL string
	// Origins are other origins the UI may be opened from, such as a dev
	// server. Passkeys are accepted from them as well as from URL.
	Origins []string
}

// resolveAddress works out the server's address from --url and --origin
// (originSet says whether --origin was given at all), the host fixed at
// init, and the port a localhost server listens on.
func resolveAddress(urlFlag, originFlag string, originSet bool, host, port string) (address, error) {
	u, err := publicURL(urlFlag, host, port)
	if err != nil {
		return address{}, err
	}
	extra := splitList(originFlag)
	if !originSet && hostName(u) == "localhost" {
		extra = []string{devOrigin}
	}
	addr := address{URL: u}
	for _, o := range extra {
		n, err := parseOrigin(o)
		if err != nil {
			return address{}, fmt.Errorf("--origin %s: %w", o, err)
		}
		if n != u && !slices.Contains(addr.Origins, n) {
			addr.Origins = append(addr.Origins, n)
		}
	}
	return addr, nil
}

// publicURL is --url, checked and written the way a browser writes an
// origin. Without --url it comes from the host fixed at init:
// https://<host>, or http://localhost:<port> for a server at localhost.
func publicURL(urlFlag, host, port string) (string, error) {
	if urlFlag != "" {
		u, err := parseOrigin(urlFlag)
		if err != nil {
			return "", fmt.Errorf("--url %s: %w", urlFlag, err)
		}
		return u, nil
	}
	raw := "https://" + host
	name, hostPort, err := net.SplitHostPort(host)
	if err != nil {
		name = host
	}
	if name == "localhost" {
		if hostPort != "" {
			port = hostPort
		}
		raw = "http://" + net.JoinHostPort("localhost", port)
	}
	u, err := parseOrigin(raw)
	if err != nil {
		return "", fmt.Errorf("no URL can be made from host %q: %w", host, err)
	}
	return u, nil
}

// parseOrigin checks that s is scheme://host[:port] with an http or https
// scheme, and returns it as a browser sends it in an Origin header: in
// lower case, without a default port or a trailing slash.
func parseOrigin(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("needs to start with http:// or https://")
	}
	if u.Hostname() == "" {
		return "", errors.New("has no host")
	}
	if u.User != nil {
		return "", errors.New("must not hold a user name or password")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("must be scheme://host[:port], without a path, query or fragment")
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if p := u.Port(); p != "" && !(u.Scheme == "http" && p == "80") && !(u.Scheme == "https" && p == "443") {
		host += ":" + p
	}
	return u.Scheme + "://" + host, nil
}

// hostName is the host of an origin, without the port or IPv6 brackets.
func hostName(origin string) string {
	u, err := url.Parse(origin)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// passkeyProblem says why browsers will not use passkeys at the URL, or
// returns "" when they will. Passkeys need a domain name as the relying
// party ID, and a secure context: https, or plain http at localhost.
func passkeyProblem(u string) string {
	name := hostName(u)
	if net.ParseIP(name) != nil {
		return "passkeys need a domain name, not an IP address"
	}
	if strings.HasPrefix(u, "http://") && name != "localhost" && !strings.HasSuffix(name, ".localhost") {
		return "browsers refuse passkeys over plain http anywhere but localhost"
	}
	return ""
}

// sshAddress is where SSH clients connect, for SSH clone URLs: --ssh-host
// if given, else the public URL's host on the port SSH listens on (left
// out when it is 22).
func sshAddress(given, publicURL string, listening net.Addr) (string, error) {
	if given != "" {
		if strings.ContainsAny(given, "/@ ") {
			return "", fmt.Errorf("--ssh-host %s: give host[:port], such as git.example.com or git.example.com:2222", given)
		}
		return given, nil
	}
	_, port, err := net.SplitHostPort(listening.String())
	if err != nil {
		return "", err
	}
	host := hostName(publicURL)
	if port == "22" {
		return host, nil
	}
	return net.JoinHostPort(host, port), nil
}
