package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"krystallos/internal/app"
	"krystallos/internal/store"
)

// visibility decides which events a subscriber may receive: events of a
// repository need repo.read, events of an org need some grant in it, and
// instance-wide events go to their own principal (or to admins).
type visibility struct {
	app   *app.App
	actor app.Actor
	cache map[string]bool
}

func (v *visibility) can(ctx context.Context, e store.Event) (bool, error) {
	if v.actor.Admin {
		return true, nil
	}
	if e.Org == "" && e.Repo == "" {
		return e.Principal == v.actor.Principal, nil
	}
	key := e.Org + "|" + e.Repo
	if ok, hit := v.cache[key]; hit {
		return ok, nil
	}
	var ok bool
	var err error
	if e.Repo != "" {
		ok, err = v.app.Can(ctx, v.app.Store.DB(), v.actor, e.Org, e.Repo, app.PermRepoRead)
	} else {
		ok, err = v.app.CanSeeOrg(ctx, v.actor, e.Org)
	}
	if err != nil {
		return false, err
	}
	v.cache[key] = ok
	return ok, nil
}

// events serves the event stream. Subscribers keep their own cursor and read
// on from it, so nothing is lost while they are away. Plain requests
// long-poll (?after=N&wait=S); with Accept: text/event-stream the response is
// a server-sent event stream that resumes from Last-Event-ID.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	actor, err := s.authenticate(r)
	if err != nil {
		writeError(w, err)
		return
	}
	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	if id := r.Header.Get("Last-Event-ID"); id != "" {
		after, _ = strconv.ParseInt(id, 10, 64)
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	vis := &visibility{app: s.App, actor: actor, cache: map[string]bool{}}
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.streamEvents(w, r, actor, vis, after)
		return
	}
	wait, _ := strconv.Atoi(q.Get("wait"))
	evs, next, err := s.pollEvents(r.Context(), actor, vis, after, limit, time.Duration(min(max(wait, 0), 60))*time.Second)
	if err != nil {
		if r.Context().Err() == nil {
			writeError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs, "next": next})
}

// pollEvents returns the events after a cursor that the actor may see,
// waiting up to wait for one to arrive. It returns the cursor to read on
// from, which moves past events the actor may not see as well. Subscribing
// is reading, so every poll is recorded like any other read.
func (s *Server) pollEvents(ctx context.Context, actor app.Actor, vis *visibility, after int64, limit int, wait time.Duration) ([]store.Event, int64, error) {
	deadline := time.Now().Add(wait)
	sub, cancel := s.App.Store.Subscribe()
	defer cancel()
	start := after
	for {
		evs, err := s.App.Store.Events(ctx, after, limit)
		if err != nil {
			return nil, after, err
		}
		visible := []store.Event{}
		for _, e := range evs {
			ok, err := vis.can(ctx, e)
			if err != nil {
				return nil, after, err
			}
			if ok {
				visible = append(visible, e)
			}
			after = e.Seq
		}
		remaining := time.Until(deadline)
		if len(visible) > 0 || len(evs) == limit || remaining <= 0 {
			s.recordRead(actor, "events.read", "", fmt.Sprintf("after=%d", start), map[string]any{"after": start, "next": after, "count": len(visible)})
			return visible, after, nil
		}
		select {
		case <-sub:
		case <-time.After(min(remaining, time.Second)):
		case <-ctx.Done():
			return nil, after, ctx.Err()
		}
	}
}

func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request, actor app.Actor, vis *visibility, after int64) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	sub, cancel := s.App.Store.Subscribe()
	defer cancel()
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		evs, err := s.App.Store.Events(r.Context(), after, 500)
		if err != nil {
			return
		}
		start, sent := after, 0
		for _, e := range evs {
			ok, err := vis.can(r.Context(), e)
			if err != nil {
				return
			}
			if ok {
				// Unnamed messages, so a client needs one handler for every
				// type, including types added later; the type is in the data.
				b, _ := json.Marshal(e)
				fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.Seq, b)
				sent++
			}
			after = e.Seq
		}
		if len(evs) > 0 {
			flusher.Flush()
			s.recordRead(actor, "events.read", "", fmt.Sprintf("after=%d", start), map[string]any{"after": start, "next": after, "count": sent, "stream": true})
		}
		if len(evs) == 500 {
			continue
		}
		select {
		case <-sub:
		case <-poll.C:
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
