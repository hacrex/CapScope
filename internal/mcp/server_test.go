package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"testing"

	"capscope/internal/auth"
	"capscope/internal/store"
)

func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/mcp_test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sessions := auth.NewSessionManager(slogDiscard())
	srv := New(st, sessions, slogDiscard())
	return srv, st
}

func slogDiscard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// authenticate runs an initialize request with a valid session token so the
// test server treats the session as authenticated (mirroring a real MCP client).
func authenticate(t *testing.T, srv *Server) string {
	t.Helper()
	token, err := srv.sessions.CreateSession(1, "tester", auth.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	params := map[string]any{
		"protocolVersion": "2024-11-05",
		"meta":            map[string]any{"token": token},
	}
	p, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	resp := srv.handle(context.Background(), Message{
		JSONRPC: "2.0", ID: jsonID(`1`), Method: "initialize", Params: p,
	})
	if resp.Error != nil {
		t.Fatalf("initialize failed: %+v", resp.Error)
	}
	return token
}

func TestServerListTools(t *testing.T) {
	srv, _ := newTestServer(t)
	authenticate(t, srv)

	msg := Message{
		JSONRPC: "2.0",
		ID:      jsonID(`1`),
		Method:  "tools/list",
		Params:  json.RawMessage(`{}`),
	}
	resp := srv.handle(context.Background(), msg)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var result struct {
		Tools []map[string]interface{} `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if len(result.Tools) != 4 {
		t.Fatalf("expected 4 tools, got %d: %+v", len(result.Tools), result.Tools)
	}
	toolNames := []string{"query_logs", "list_snapshots", "get_snapshot", "run_sql"}
	found := make(map[string]bool)
	for _, tool := range result.Tools {
		found[tool["name"].(string)] = true
	}
	for _, name := range toolNames {
		if !found[name] {
			t.Errorf("tool %q not found", name)
		}
	}
}

func TestServerRunSQL(t *testing.T) {
	srv, st := newTestServer(t)
	authenticate(t, srv)

	// Insert a test row directly
	_, err := st.CreateUser("testuser", "hash", "admin")
	if err != nil {
		t.Fatal(err)
	}

	msg := Message{
		JSONRPC: "2.0",
		ID:      jsonID(`1`),
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "run_sql",
			"arguments": {"sql": "SELECT name FROM users"}
		}`),
	}
	resp := srv.handle(context.Background(), msg)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var result struct {
		Columns []string         `json:"columns"`
		Rows    []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if len(result.Columns) == 0 {
		t.Fatal("expected columns in result")
	}
	if len(result.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(result.Rows))
	}
	if result.Rows[0]["name"] != "testuser" {
		t.Errorf("expected name=testuser, got %v", result.Rows[0]["name"])
	}
}

func TestServerRunSQLRejectsWrite(t *testing.T) {
	srv, _ := newTestServer(t)
	authenticate(t, srv)

	msg := Message{
		JSONRPC: "2.0",
		ID:      jsonID(`1`),
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "run_sql",
			"arguments": {"sql": "DELETE FROM users"}
		}`),
	}
	resp := srv.handle(context.Background(), msg)
	if resp.Error == nil {
		t.Fatal("expected error for DELETE statement")
	}
	// validateSQL returns ErrInternal for invalid SQL
	if resp.Error.Code != ErrInternal {
		t.Fatalf("expected ErrInternal, got %+v", resp.Error)
	}
}

func TestServerRunSQLRejectsInsert(t *testing.T) {
	srv, _ := newTestServer(t)
	authenticate(t, srv)

	msg := Message{
		JSONRPC: "2.0",
		ID:      jsonID(`1`),
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "run_sql",
			"arguments": {"sql": "INSERT INTO users (name, password_hash, role) VALUES ('x', 'y', 'viewer')"}
		}`),
	}
	resp := srv.handle(context.Background(), msg)
	if resp.Error == nil {
		t.Fatal("expected error for INSERT statement")
	}
}

func TestServerGetSnapshot(t *testing.T) {
	srv, st := newTestServer(t)
	authenticate(t, srv)

	// Insert a snapshot directly
	sn := store.Snapshot{
		TS:           1000,
		Kind:         "crash",
		ContainerID:  "c1",
		Container:    "web.1",
		App:          "web",
		Image:        "nginx:1.25",
		ExitCode:     1,
		OOMKilled:    false,
		RestartCount: 0,
		UptimeSec:    60,
		Summary:      "test crash",
	}
	id, err := st.InsertSnapshot(sn)
	if err != nil {
		t.Fatal(err)
	}

	msg := Message{
		JSONRPC: "2.0",
		ID:      jsonID(`1`),
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "get_snapshot",
			"arguments": {"id": ` + strconv.FormatInt(id, 10) + `}
		}`),
	}
	resp := srv.handle(context.Background(), msg)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var result struct {
		ID       int64  `json:"id"`
		App      string `json:"app"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if result.ID != id {
		t.Fatalf("expected id=%d, got %d", id, result.ID)
	}
	if result.App != "web" {
		t.Errorf("expected app=web, got %s", result.App)
	}
}

func TestServerAuthRequired(t *testing.T) {
	// When auth is on (sessions not nil), unauthenticated initialize should fail
	srv, _ := newTestServer(t)

	msg := Message{
		JSONRPC: "2.0",
		ID:      jsonID(`1`),
		Method:  "initialize",
		Params:  json.RawMessage(`{}`),
	}
	resp := srv.handle(context.Background(), msg)
	if resp.Error == nil {
		t.Fatal("expected auth error for unauthenticated initialize")
	}
	if resp.Error.Code != ErrUnauthorized {
		t.Fatalf("expected ErrUnauthorized, got %+v", resp.Error)
	}
}

func TestServerAuthRequiredTools(t *testing.T) {
	// When auth is on, tools must not be callable without a successful initialize.
	srv, _ := newTestServer(t)
	for _, method := range []string{"tools/list", "tools/call"} {
		msg := Message{
			JSONRPC: "2.0", ID: jsonID(`1`), Method: method,
			Params: json.RawMessage(`{"name":"run_sql","arguments":{"sql":"SELECT 1"}}`),
		}
		resp := srv.handle(context.Background(), msg)
		if resp.Error == nil || resp.Error.Code != ErrUnauthorized {
			t.Fatalf("method %s: expected ErrUnauthorized, got %+v", method, resp.Error)
		}
	}
}

func jsonID(s string) *json.RawMessage {
	r := json.RawMessage(s)
	return &r
}
