package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"capscope/internal/store"
)

func TestDeploysRecordAndList(t *testing.T) {
	srv, st, _ := setup(t)

	// Record a deploy via POST.
	body, _ := json.Marshal(map[string]any{"app": "web", "version": "v1.2.3", "note": "rollout"})
	req, _ := http.NewRequest("POST", srv.URL+"/api/deploys", bytes.NewReader(body))
	req.SetBasicAuth("u", "p")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("record status %d: %s", resp.StatusCode, b)
	}
	var created struct {
		ID      int64  `json:"id"`
		TS      int64  `json:"ts"`
		Version string `json:"version"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	if created.ID == 0 || created.Version != "v1.2.3" {
		t.Fatalf("created = %+v", created)
	}

	// It shows up in the list.
	var list struct {
		Deploys []store.Deploy `json:"deploys"`
	}
	r := get(t, srv.URL+"/api/deploys?app=web", true)
	defer r.Body.Close()
	_ = json.NewDecoder(r.Body).Decode(&list)
	if len(list.Deploys) != 1 || list.Deploys[0].Version != "v1.2.3" || list.Deploys[0].Note != "rollout" {
		t.Fatalf("list = %+v", list.Deploys)
	}

	// App filter excludes others.
	if _, err := st.RecordDeploy(store.Deploy{TS: time.Now().UnixMilli(), App: "api", Version: "v9"}); err != nil {
		t.Fatal(err)
	}
	r2 := get(t, srv.URL+"/api/deploys?app=web", true)
	defer r2.Body.Close()
	_ = json.NewDecoder(r2.Body).Decode(&list)
	if len(list.Deploys) != 1 {
		t.Fatalf("app filter: %+v", list.Deploys)
	}
}

func TestDeploysValidation(t *testing.T) {
	srv, _, _ := setup(t)
	// Missing app -> 400.
	body, _ := json.Marshal(map[string]any{"version": "v1"})
	req, _ := http.NewRequest("POST", srv.URL+"/api/deploys", bytes.NewReader(body))
	req.SetBasicAuth("u", "p")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("want 400 for missing app, got %d", resp.StatusCode)
	}
	// Bad JSON -> 400.
	req2, _ := http.NewRequest("POST", srv.URL+"/api/deploys", strings.NewReader("{nope"))
	req2.SetBasicAuth("u", "p")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Errorf("want 400 for bad json, got %d", resp2.StatusCode)
	}
}

func TestSnapshotShowsPrecedingDeploy(t *testing.T) {
	srv, st, _ := setup(t)
	now := time.Now().UnixMilli()
	// A deploy 5 minutes before the crash.
	if _, err := st.RecordDeploy(store.Deploy{TS: now - 5*60_000, App: "web", Version: "v2.0"}); err != nil {
		t.Fatal(err)
	}
	id, err := st.InsertSnapshot(store.Snapshot{TS: now, DieTS: now, Kind: "crash", App: "web", Container: "web.1", ExitCode: 1, Summary: "s"})
	if err != nil {
		t.Fatal(err)
	}

	r := get(t, srv.URL+"/api/snapshots/"+itoa(id), true)
	defer r.Body.Close()
	var sn store.Snapshot
	_ = json.NewDecoder(r.Body).Decode(&sn)
	if sn.Deploy == nil || sn.Deploy.Version != "v2.0" {
		t.Fatalf("deploy not attached: %+v", sn.Deploy)
	}
	// Markdown export mentions it.
	re := get(t, srv.URL+"/api/snapshots/"+itoa(id)+"/export", true)
	defer re.Body.Close()
	b, _ := io.ReadAll(re.Body)
	if !strings.Contains(string(b), "v2.0") {
		t.Errorf("markdown missing deploy: %s", b)
	}
}

func TestSnapshotNoDeployWhenNoneRecorded(t *testing.T) {
	srv, st, _ := setup(t)
	now := time.Now().UnixMilli()
	id, err := st.InsertSnapshot(store.Snapshot{TS: now, DieTS: now, Kind: "crash", App: "web", Container: "web.1", ExitCode: 1, Summary: "s"})
	if err != nil {
		t.Fatal(err)
	}
	r := get(t, srv.URL+"/api/snapshots/"+itoa(id), true)
	defer r.Body.Close()
	var sn store.Snapshot
	_ = json.NewDecoder(r.Body).Decode(&sn)
	if sn.Deploy != nil {
		t.Fatalf("want no deploy, got %+v", sn.Deploy)
	}
}
