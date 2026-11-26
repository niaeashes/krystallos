package server

import (
	"net/http"
	"strings"

	"krystallos/internal/app"
)

// Passkey sign-in. These endpoints need no credential: the passkey or the
// one-time link is the proof.

func (s *Server) registerBegin(w http.ResponseWriter, r *http.Request) {
	in, err := decode[struct {
		Token string `json:"token"`
		Name  string `json:"name"`
	}](r)
	if err != nil {
		writeError(w, err)
		return
	}
	id, opts, err := s.App.BeginPasskeyRegistration(r.Context(), in.Token, in.Name)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ceremony": id, "options": opts})
}

func (s *Server) registerFinish(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	actor, err := s.App.FinishPasskeyRegistration(r.Context(), r.URL.Query().Get("ceremony"), body)
	if err != nil {
		writeError(w, err)
		return
	}
	s.startSession(w, r, actor, "passkey-registration")
}

// inspectLink says what a one-time link is for, so the page can ask for a
// user name when it is an invitation.
func (s *Server) inspectLink(w http.ResponseWriter, r *http.Request) {
	in, err := decode[struct {
		Token string `json:"token"`
	}](r)
	if err != nil {
		writeError(w, err)
		return
	}
	info, err := s.App.InspectLink(r.Context(), in.Token)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) loginBegin(w http.ResponseWriter, r *http.Request) {
	id, opts, err := s.App.BeginPasskeyLogin(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ceremony": id, "options": opts})
}

func (s *Server) loginFinish(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	actor, err := s.App.FinishPasskeyLogin(r.Context(), r.URL.Query().Get("ceremony"), body)
	if err != nil {
		writeError(w, err)
		return
	}
	s.startSession(w, r, actor, "passkey")
}

// loginLink redeems a one-time sign-in link from the admin CLI: the way
// back in for someone who lost their passkey.
func (s *Server) loginLink(w http.ResponseWriter, r *http.Request) {
	in, err := decode[struct {
		Token string `json:"token"`
	}](r)
	if err != nil {
		writeError(w, err)
		return
	}
	actor, err := s.App.RedeemLoginLink(r.Context(), in.Token)
	if err != nil {
		writeError(w, err)
		return
	}
	s.startSession(w, r, actor, "admin-link")
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		actor, err := s.authenticate(r)
		if err != nil {
			writeError(w, err)
			return
		}
		if err := s.App.EndBrowserSession(r.Context(), actor, c.Value); err != nil {
			writeError(w, err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: s.secure()})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, actor app.Actor, method string) {
	sess, err := s.App.StartBrowserSession(r.Context(), actor, method)
	if err != nil {
		writeError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: sess.Token, Path: "/", Expires: sess.Expires,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: s.secure(),
	})
	out, _ := s.me(r, actor)
	writeJSON(w, http.StatusOK, out)
}

// secure says whether the session cookie is sent over https only: when
// the server's public URL is https, whatever a request says about itself.
func (s *Server) secure() bool {
	return strings.HasPrefix(s.URL, "https://")
}
