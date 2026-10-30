package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"capscope/internal/store"
)

// deploysList returns deploy markers, optionally filtered to one app and a
// time range. Used by the charts and the log timeline to draw markers (V9).
func (s *Server) deploysList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	until, _ := strconv.ParseInt(q.Get("until"), 10, 64)
	list, err := s.st.ListDeploys(r.Context(), q.Get("app"), since, until, limit)
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"deploys": list})
}

// deploysCreate records one marker. A CI pipeline can POST here on release;
// the UI's "Record deploy" button does the same.
func (s *Server) deploysCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		App     string `json:"app"`
		Version string `json:"version"`
		Note    string `json:"note"`
		TS      int64  `json:"ts"` // optional unix ms; 0 means now
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, 400, err)
		return
	}
	if in.App == "" {
		fail(w, 400, errors.New("app is required"))
		return
	}
	if in.TS <= 0 || in.TS > time.Now().UnixMilli()+60_000 {
		in.TS = time.Now().UnixMilli() // ignore far-future or bogus clocks
	}
	id, err := s.st.RecordDeploy(store.Deploy{TS: in.TS, App: in.App, Version: in.Version, Note: in.Note})
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"id": id, "ts": in.TS, "app": in.App, "version": in.Version, "note": in.Note})
}
