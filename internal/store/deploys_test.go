package store

import (
	"context"
	"testing"
	"time"
)

func TestRecordAndListDeploys(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	id, err := s.RecordDeploy(Deploy{TS: now, App: "web", Version: "v1", Note: "first"})
	if err != nil || id == 0 {
		t.Fatalf("record: id=%d err=%v", id, err)
	}
	// ts 0 means now.
	id2, err := s.RecordDeploy(Deploy{App: "web", Version: "v2"})
	if err != nil || id2 <= id {
		t.Fatalf("record now: id=%d err=%v", id2, err)
	}
	if _, err := s.RecordDeploy(Deploy{TS: now, App: "api", Version: "v9"}); err != nil {
		t.Fatal(err)
	}

	list, err := s.ListDeploys(ctx, "web", 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("web deploys = %d, want 2", len(list))
	}
	if list[0].Version != "v2" || list[1].Version != "v1" {
		t.Errorf("order (newest first): %+v", list)
	}

	all, err := s.ListDeploys(ctx, "", 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("all deploys = %d, want 3", len(all))
	}

	// Time-window filter.
	windowed, err := s.ListDeploys(ctx, "web", now+1, 0, 100) // since in the future
	if err != nil {
		t.Fatal(err)
	}
	if len(windowed) != 0 {
		t.Errorf("future window = %d, want 0", len(windowed))
	}
}

func TestListDeploysLimitClamped(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := s.RecordDeploy(Deploy{TS: int64(i + 1), App: "a", Version: "v"}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListDeploys(ctx, "", 0, 0, 0) // default 200
	if err != nil || len(list) != 5 {
		t.Fatalf("default limit: %d err=%v", len(list), err)
	}
	list, _ = s.ListDeploys(ctx, "", 0, 0, 2)
	if len(list) != 2 {
		t.Errorf("limit=2 -> %d", len(list))
	}
}

func TestDeployBefore(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	base := time.Now().UnixMilli()

	// None recorded yet.
	if d, err := s.DeployBefore(ctx, "web", base); err != nil || d.ID != 0 {
		t.Fatalf("empty: d=%+v err=%v", d, err)
	}

	s.RecordDeploy(Deploy{TS: base - 3600_000, App: "web", Version: "old"})
	s.RecordDeploy(Deploy{TS: base - 60_000, App: "web", Version: "new"})
	s.RecordDeploy(Deploy{TS: base - 60_000, App: "api", Version: "other"})

	// Newest web deploy at or before the crash time.
	d, err := s.DeployBefore(ctx, "web", base)
	if err != nil {
		t.Fatal(err)
	}
	if d.Version != "new" {
		t.Errorf("deploy before now = %q, want new", d.Version)
	}

	// A crash before both deploys sees none.
	d, _ = s.DeployBefore(ctx, "web", base-7200_000)
	if d.ID != 0 {
		t.Errorf("deploy before ancient crash = %+v, want none", d)
	}

	// Scoped to the app.
	d, _ = s.DeployBefore(ctx, "api", base)
	if d.Version != "other" {
		t.Errorf("api deploy = %q, want other", d.Version)
	}
}
