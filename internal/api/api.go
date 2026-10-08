// Package api serves EffectTrace's read-only HTTP API.
package api

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/effecttrace/effecttrace/internal/correlate"
	"github.com/effecttrace/effecttrace/internal/render"
	"github.com/effecttrace/effecttrace/internal/store"
	"github.com/effecttrace/effecttrace/pkg/model"
)

var (
	actionIDRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,200}$`)
	traceIDRE  = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// Server holds API dependencies.
type Server struct {
	Engine  *correlate.Engine
	Store   *store.Store
	Version string
	// Token, when non-empty, is required as a Bearer token on /api/*.
	Token string
	// Ready reports readiness for /readyz.
	Ready func() bool
	// Now returns the current time.
	Now func() time.Time
	// ObserveBuild records graph build durations.
	ObserveBuild func(time.Duration)
}

// Handler returns the API routes. metrics may be nil.
func (s *Server) Handler(metrics http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if s.Ready != nil && !s.Ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	if metrics != nil {
		mux.Handle("GET /metrics", metrics)
	}
	mux.HandleFunc("GET /api/v1/status", s.auth(s.status))
	mux.HandleFunc("GET /api/v1/actions", s.auth(s.actions))
	mux.HandleFunc("GET /api/v1/effects/{id}", s.auth(s.effects))
	mux.HandleFunc("GET /api/v1/effects/{id}/graph", s.auth(s.graph))
	mux.HandleFunc("GET /api/v1/traces/{traceID}/actions", s.auth(s.traceActions))
	return securityHeaders(mux)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		h.ServeHTTP(w, r)
	})
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Token != "" {
			got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
		}
		h(w, r)
	}
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	type src struct {
		Source    string    `json:"source"`
		Healthy   bool      `json:"healthy"`
		Detail    string    `json:"detail,omitempty"`
		At        time.Time `json:"lastReport"`
		StartedAt time.Time `json:"startedAt,omitzero"`
	}
	out := struct {
		Version string         `json:"version"`
		Sources []src          `json:"sources"`
		Counts  map[string]int `json:"counts"`
	}{Version: s.Version}
	s.Store.Read(func(v store.View) {
		for _, name := range []string{"kubernetes-audit", "kubernetes-watch", "kubernetes-events", "otlp", "prometheus"} {
			if st, ok := v.Source(name); ok {
				out.Sources = append(out.Sources, src{Source: name, Healthy: st.Healthy, Detail: st.Detail, At: st.At, StartedAt: st.StartedAt})
			} else {
				out.Sources = append(out.Sources, src{Source: name, Detail: "not reported"})
			}
		}
		out.Counts = v.Counts()
	})
	writeJSON(w, out)
}

func (s *Server) actions(w http.ResponseWriter, r *http.Request) {
	since := time.Time{}
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since must be RFC 3339")
			return
		}
		since = t
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			writeError(w, http.StatusBadRequest, "limit must be 1..1000")
			return
		}
		limit = n
	}
	list := s.Engine.Actions(s.now(), since)
	if len(list) > limit {
		list = list[len(list)-limit:]
	}
	if list == nil {
		list = []correlate.ActionSummary{}
	}
	writeJSON(w, map[string]any{"actions": list})
}

func (s *Server) build(w http.ResponseWriter, r *http.Request) (*model.EffectGraph, bool) {
	id := r.PathValue("id")
	if !actionIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid action id")
		return nil, false
	}
	start := time.Now()
	g, err := s.Engine.Graph(s.now(), id)
	if s.ObserveBuild != nil {
		s.ObserveBuild(time.Since(start))
	}
	if errors.Is(err, correlate.ErrNotFound) {
		writeError(w, http.StatusNotFound, "action not found")
		return nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "graph build failed")
		return nil, false
	}
	return g, true
}

func (s *Server) effects(w http.ResponseWriter, r *http.Request) {
	g, ok := s.build(w, r)
	if !ok {
		return
	}
	b, err := g.MarshalCanonical()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "graph failed validation")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func (s *Server) graph(w http.ResponseWriter, r *http.Request) {
	g, ok := s.build(w, r)
	if !ok {
		return
	}
	var buf bytes.Buffer
	switch r.URL.Query().Get("format") {
	case "", "text":
		render.Explain(&buf, g, render.Options{Verbose: r.URL.Query().Get("verbose") == "true"})
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	case "dot":
		render.DOT(&buf, g)
		w.Header().Set("Content-Type", "text/vnd.graphviz; charset=utf-8")
	case "mermaid":
		render.Mermaid(&buf, g)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	default:
		writeError(w, http.StatusBadRequest, "format must be text, dot or mermaid")
		return
	}
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) traceActions(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(r.PathValue("traceID"))
	if !traceIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid trace id")
		return
	}
	ids := s.Engine.ActionsForTrace(s.now(), id)
	if ids == nil {
		ids = []string{}
	}
	writeJSON(w, map[string]any{"actions": ids})
}
