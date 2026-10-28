// Package api serves the JSON/SSE API and the embedded web UI.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"capscope/internal/collector"
	"capscope/internal/config"
	"capscope/internal/hub"
	"capscope/internal/store"
	"capscope/internal/web"
)

type Server struct {
	cfg config.Config
	st  *store.Store
	mgr *collector.Manager
	hub *hub.Hub
	log *slog.Logger
}

func New(cfg config.Config, st *store.Store, mgr *collector.Manager, h *hub.Hub, log *slog.Logger) *Server {
	return &Server{cfg: cfg, st: st, mgr: mgr, hub: h, log: log}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	mux.HandleFunc("GET /api/containers", s.containers)
	mux.HandleFunc("GET /api/apps", s.apps)
	mux.HandleFunc("GET /api/logs", s.logs)
	mux.HandleFunc("GET /api/logs/stream", s.logStream)
	mux.HandleFunc("GET /api/sql", s.sql)
	mux.HandleFunc("GET /api/metrics", s.metrics)
	mux.HandleFunc("GET /api/snapshots", s.snapshots)
	mux.HandleFunc("GET /api/snapshots/{id}", s.snapshot)
	mux.HandleFunc("GET /api/snapshots/{id}/export", s.snapshotExport)
	sub, _ := fs.Sub(web.FS, "static")
	mux.Handle("GET /", http.FileServerFS(sub))
	return s.auth(mux)
}

func (s *Server) auth(next http.Handler) http.Handler {
	if s.cfg.AuthPass == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		u, p, ok := r.BasicAuth()
		okU := subtle.ConstantTimeCompare([]byte(u), []byte(s.cfg.AuthUser))
		okP := subtle.ConstantTimeCompare([]byte(p), []byte(s.cfg.AuthPass))
		if !ok || okU&okP != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="capscope"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	http.Error(w, err.Error(), code)
}

func (s *Server) containers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"containers": s.mgr.Containers()})
}

func (s *Server) apps(w http.ResponseWriter, r *http.Request) {
	a, err := s.st.AppSummaries(r.Context())
	if err != nil {
		fail(w, 500, err)
		return
	}
	if a == nil {
		a = []store.AppSummary{}
	}
	writeJSON(w, map[string]any{"apps": a})
}

// parseLogQuery reads the shared filter parameters used by /api/logs and /api/logs/stream.
func parseLogQuery(r *http.Request) (store.LogQuery, error) {
	v := r.URL.Query()
	q := store.LogQuery{
		App:       v.Get("app"),
		Container: v.Get("container"),
		Q:         v.Get("q"),
	}
	switch v.Get("stream") {
	case "stdout", "o":
		q.Stream = "o"
	case "stderr", "e":
		q.Stream = "e"
	}
	if l := v.Get("level"); l != "" {
		for _, x := range strings.Split(l, ",") {
			if x = strings.TrimSpace(x); x != "" {
				q.Levels = append(q.Levels, x)
			}
		}
	}
	if b, _ := strconv.ParseBool(v.Get("regex")); b && q.Q != "" {
		re, err := regexp.Compile("(?i)" + q.Q)
		if err != nil {
			return q, fmt.Errorf("invalid regex: %w", err)
		}
		q.Regex = re
	}
	q.SinceMs = parseSince(v.Get("since"))
	q.UntilMs, _ = strconv.ParseInt(v.Get("until"), 10, 64)
	q.AfterID, _ = strconv.ParseInt(v.Get("after_id"), 10, 64)
	q.BeforeID, _ = strconv.ParseInt(v.Get("before_id"), 10, 64)
	q.Limit, _ = strconv.Atoi(v.Get("limit"))
	return q, nil
}

// parseSince accepts a relative duration ("15m", "2h", "1d") or an epoch in milliseconds.
func parseSince(s string) int64 {
	if s == "" {
		return 0
	}
	if d, err := config.ParseDuration(s); err == nil {
		return time.Now().Add(-d).UnixMilli()
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	q, err := parseLogQuery(r)
	if err != nil {
		fail(w, 400, err)
		return
	}
	rows, err := s.st.QueryLogs(r.Context(), q)
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"logs": rows})
}

// sql runs one read-only ad-hoc statement over the stored tables. Validation
// and the row cap live in store.QuerySQL; a 5s deadline keeps a heavy scan
// from pinning the read pool.
func (s *Server) sql(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	res, err := s.st.QuerySQL(ctx, r.URL.Query().Get("query"), limit)
	if err != nil {
		if errors.Is(err, store.ErrInvalidQuery) {
			fail(w, 400, err)
		} else {
			fail(w, 500, err)
		}
		return
	}
	writeJSON(w, res)
}

// logStream pushes new rows as Server-Sent Events. Rows arrive after they are committed, so each has an id.
func (s *Server) logStream(w http.ResponseWriter, r *http.Request) {
	q, err := parseLogQuery(r)
	if err != nil {
		fail(w, 400, err)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, fmt.Errorf("streaming unsupported"))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // keep nginx (CapRover) from buffering the stream
	sub := s.hub.Subscribe(q)
	defer s.hub.Unsubscribe(sub)
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	var lastDropped int64
	for {
		select {
		case <-r.Context().Done():
			return
		case row := <-sub.C:
			b, _ := json.Marshal(row)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		case <-ping.C:
			if d := sub.Dropped.Load(); d != lastDropped {
				lastDropped = d
				fmt.Fprintf(w, "event: dropped\ndata: %d\n\n", d)
			} else {
				fmt.Fprint(w, ": ping\n\n")
			}
			fl.Flush()
		}
	}
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	rng := time.Hour
	if d, err := config.ParseDuration(v.Get("range")); err == nil && d > 0 {
		rng = d
	}
	bucket := rng.Milliseconds() / 300
	if bucket < 5000 {
		bucket = 5000
	}
	since := time.Now().Add(-rng).UnixMilli()
	pts, err := s.st.QueryMetrics(r.Context(), v.Get("app"), v.Get("container"), since, 0, bucket)
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"bucket_ms": bucket, "points": pts})
}

func (s *Server) snapshots(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.st.ListSnapshots(r.Context(), r.URL.Query().Get("app"), limit)
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"snapshots": list})
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, 400, err)
		return
	}
	sn, err := s.st.GetSnapshot(r.Context(), id)
	if err != nil {
		fail(w, 404, err)
		return
	}
	writeJSON(w, sn)
}

func (s *Server) snapshotExport(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, 400, err)
		return
	}
	sn, err := s.st.GetSnapshot(r.Context(), id)
	if err != nil {
		fail(w, 404, err)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="crash-%s-%d.md"`, sn.App, sn.ID))
	fmt.Fprint(w, SnapshotMarkdown(sn))
}

// SnapshotMarkdown renders a snapshot as an incident-handoff document.
func SnapshotMarkdown(sn store.Snapshot) string {
	var b strings.Builder
	t := time.UnixMilli(sn.TS).UTC().Format(time.RFC3339)
	fmt.Fprintf(&b, "# Crash report: %s\n\n", sn.App)
	fmt.Fprintf(&b, "- Time: %s\n- Kind: %s\n- Container: %s\n- Image: %s\n- Exit code: %d\n- OOM killed: %v\n- Restart count: %d\n- Uptime before death: %ds\n",
		t, sn.Kind, sn.Container, sn.Image, sn.ExitCode, sn.OOMKilled, sn.RestartCount, sn.UptimeSec)
	if sn.MemLimit > 0 {
		fmt.Fprintf(&b, "- Memory limit: %d bytes\n", sn.MemLimit)
	}
	if sn.Error != "" {
		fmt.Fprintf(&b, "- Docker error: %s\n", sn.Error)
	}
	fmt.Fprintf(&b, "\n## Summary\n\n%s\n", sn.Summary)
	if len(sn.Metrics) > 0 {
		fmt.Fprintf(&b, "\n## Resource trend (last %d samples)\n\n| time (UTC) | cpu %% | memory MiB |\n|---|---|---|\n", len(sn.Metrics))
		for _, m := range sn.Metrics {
			fmt.Fprintf(&b, "| %s | %.1f | %.1f |\n", time.UnixMilli(m.TS).UTC().Format("15:04:05"), m.CPU, float64(m.Mem)/1048576)
		}
	}
	fmt.Fprintf(&b, "\n## Last %d log lines\n\n```\n", len(sn.Logs))
	for _, l := range sn.Logs {
		st := "out"
		if l.Stream == "e" {
			st = "err"
		}
		fmt.Fprintf(&b, "%s %s %s\n", time.UnixMilli(l.TSMs).UTC().Format("15:04:05.000"), st, l.Line)
	}
	b.WriteString("```\n")
	return b.String()
}
