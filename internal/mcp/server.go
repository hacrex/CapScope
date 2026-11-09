// Package mcp implements the Model Context Protocol (MCP) server over stdio
// for CapScope (Phase 6.4). It exposes read-only tools over the store:
//
//	query_logs     — query container logs with filters
//	list_snapshots — list crash snapshots
//	get_snapshot   — get a specific crash snapshot
//	run_sql        — run a read-only SQL query
//
// The server authenticates via a bearer session token passed in the
// `initialize` request (meta.token). Auth is skipped when no AUTH_PASS is
// configured (auth disabled) or when AUTH_PROVIDER=proxy (sessionless).
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"capscope/internal/auth"
	"capscope/internal/store"
)

// Server is the MCP server over stdio.
type Server struct {
	st       *store.Store
	sessions *auth.SessionManager
	log      *slog.Logger
	authOn   bool // true when sessions manager exists (auth required)
	authed   bool // true once a successful initialize authenticated the session (or auth not required)
}

// New creates an MCP server over the store.
func New(st *store.Store, sessions *auth.SessionManager, log *slog.Logger) *Server {
	return &Server{st: st, sessions: sessions, log: log, authOn: sessions != nil, authed: sessions == nil}
}

// Run serves the MCP protocol over stdin/stdout until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	dec := json.NewDecoder(os.Stdin)
	enc := json.NewEncoder(os.Stdout)

	// Decode loop runs in a goroutine so ctx cancellation can interrupt it.
	type msgResult struct {
		msg Message
		err error
	}
	msgCh := make(chan msgResult, 1)

	go func() {
		for {
			if ctx.Err() != nil {
				return // shutdown: don't block forever in dec.Decode (goroutine leak)
			}
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				msgCh <- msgResult{err: err}
				return
			}
			var msg Message
			if err := json.Unmarshal(raw, &msg); err != nil {
				msgCh <- msgResult{err: err}
				return
			}
			msgCh <- msgResult{msg: msg}
		}
	}()

	decodeErrs := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case result := <-msgCh:
			if result.err != nil {
				if errors.Is(result.err, io.EOF) || errors.Is(result.err, io.ErrUnexpectedEOF) {
					return nil
				}
				decodeErrs++
				s.log.Warn("mcp: decode error", "err", result.err)
				if decodeErrs >= 5 {
					s.log.Warn("mcp: too many decode errors, stopping")
					return nil
				}
				continue
			}
			decodeErrs = 0
			resp := s.handle(ctx, result.msg)
			if resp.ID != nil || result.msg.ID != nil || result.msg.Method == "initialize" || result.msg.Method == "" {
				if err := enc.Encode(resp); err != nil {
					s.log.Warn("mcp: encode error", "err", err)
					return err
				}
			}
		}
	}
}

// Message is a JSON-RPC 2.0 message.
type Message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *Error           `json:"error,omitempty"`
}

// Error is a JSON-RPC 2.0 error.
type Error struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

const (
	ErrParse        = -32700
	ErrInvalid      = -32600
	ErrMethod       = -32601
	ErrParams       = -32602
	ErrInternal     = -32603
	ErrUnauthorized = -32001
)

func (s *Server) handle(ctx context.Context, msg Message) Message {
	if msg.JSONRPC != "2.0" {
		return errorResponse(nil, ErrInvalid, "invalid JSON-RPC version", nil)
	}

	switch msg.Method {
	case "initialize":
		return s.handleInitialize(msg)
	case "initialized":
		// Notification: no response
		return Message{JSONRPC: "2.0", ID: msg.ID}
	case "tools/list":
		if !s.authed {
			return errorResponse(msg.ID, ErrUnauthorized, "session token required", nil)
		}
		return s.handleToolsList(msg)
	case "tools/call":
		if !s.authed {
			return errorResponse(msg.ID, ErrUnauthorized, "session token required", nil)
		}
		return s.handleToolsCall(ctx, msg)
	case "ping":
		return Message{JSONRPC: "2.0", ID: msg.ID, Result: json.RawMessage(`{}`)}
	default:
		return errorResponse(msg.ID, ErrMethod, "method not found", nil)
	}
}

func errorResponse(id *json.RawMessage, code int, message string, data interface{}) Message {
	dataBytes, _ := json.Marshal(data)
	return Message{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &Error{Code: code, Message: message, Data: dataBytes},
	}
}

func resultResponse(id *json.RawMessage, result interface{}) Message {
	b, _ := json.Marshal(result)
	return Message{JSONRPC: "2.0", ID: id, Result: b}
}

// handleInitialize processes the initialize request. The client may include
// a session token in params.meta.token for authentication.
func (s *Server) handleInitialize(msg Message) Message {
	var params struct {
		ProtocolVersion string          `json:"protocolVersion"`
		Capabilities    json.RawMessage `json:"capabilities"`
		ClientInfo      json.RawMessage `json:"clientInfo"`
		Meta            json.RawMessage `json:"meta,omitempty"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return errorResponse(msg.ID, ErrParams, "invalid params", nil)
	}

	// Auth: check session token from meta.token
	if s.authOn {
		var meta struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(params.Meta, &meta); err == nil && meta.Token != "" {
			userID, name, role, ok := s.sessions.Validate(meta.Token)
			if !ok {
				return errorResponse(msg.ID, ErrUnauthorized, "invalid or expired session token", nil)
			}
			s.log.Info("mcp: authenticated session", "user", name, "role", role, "userID", userID)
			s.authed = true
		} else {
			return errorResponse(msg.ID, ErrUnauthorized, "session token required", nil)
		}
	}

	return resultResponse(msg.ID, map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"serverInfo": map[string]string{
			"name":    "capscope",
			"version": "0.14.0",
		},
		"capabilities": map[string]interface{}{
			"tools": map[string]bool{"listChanged": false},
		},
	})
}

// handleToolsList returns the available MCP tools.
func (s *Server) handleToolsList(msg Message) Message {
	tools := []map[string]interface{}{
		{
			"name":        "query_logs",
			"description": "Query container logs with optional filters. Returns log rows matching the criteria.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"app":       map[string]string{"type": "string", "description": "Filter by app name"},
					"container": map[string]string{"type": "string", "description": "Filter by container name or ID"},
					"host":      map[string]string{"type": "string", "description": "Filter by host ID"},
					"q":         map[string]string{"type": "string", "description": "Full-text search query"},
					"level":     map[string]string{"type": "string", "description": "Filter by log level (error, warn, info, debug)"},
					"stream":    map[string]string{"type": "string", "description": "Filter by stream (o=stdout, e=stderr)"},
					"since":     map[string]string{"type": "string", "description": "Start time (RFC3339)"},
					"until":     map[string]string{"type": "string", "description": "End time (RFC3339)"},
					"limit":     map[string]string{"type": "integer", "description": "Maximum rows (default 500, max 5000)"},
				},
			},
		},
		{
			"name":        "list_snapshots",
			"description": "List crash snapshots, optionally filtered by app or host.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"app":   map[string]string{"type": "string", "description": "Filter by app name"},
					"host":  map[string]string{"type": "string", "description": "Filter by host ID"},
					"limit": map[string]string{"type": "integer", "description": "Maximum results (default 100)"},
				},
			},
		},
		{
			"name":        "get_snapshot",
			"description": "Get a specific crash snapshot by ID, including its log lines and metrics.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"id": map[string]string{"type": "integer", "description": "Snapshot ID"},
				},
				"required": []string{"id"},
			},
		},
		{
			"name":        "run_sql",
			"description": "Run a read-only SQL query against the CapScope SQLite store. Only SELECT statements are allowed.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"sql": map[string]string{"type": "string", "description": "SQL SELECT query"},
				},
				"required": []string{"sql"},
			},
		},
	}
	return resultResponse(msg.ID, map[string]interface{}{"tools": tools})
}

// handleToolsCall executes a tool call.
func (s *Server) handleToolsCall(ctx context.Context, msg Message) Message {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return errorResponse(msg.ID, ErrParams, "invalid params", nil)
	}

	switch params.Name {
	case "query_logs":
		return s.handleQueryLogs(ctx, msg.ID, params.Arguments)
	case "list_snapshots":
		return s.handleListSnapshots(ctx, msg.ID, params.Arguments)
	case "get_snapshot":
		return s.handleGetSnapshot(ctx, msg.ID, params.Arguments)
	case "run_sql":
		return s.handleRunSQL(ctx, msg.ID, params.Arguments)
	default:
		return errorResponse(msg.ID, ErrMethod, "unknown tool: "+params.Name, nil)
	}
}

// handleQueryLogs handles the query_logs tool.
func (s *Server) handleQueryLogs(ctx context.Context, id *json.RawMessage, args json.RawMessage) Message {
	var params struct {
		App       string `json:"app,omitempty"`
		Container string `json:"container,omitempty"`
		Host      string `json:"host,omitempty"`
		Q         string `json:"q,omitempty"`
		Level     string `json:"level,omitempty"`
		Stream    string `json:"stream,omitempty"`
		Since     string `json:"since,omitempty"`
		Until     string `json:"until,omitempty"`
		Limit     int    `json:"limit,omitempty"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return errorResponse(id, ErrParams, "invalid arguments", nil)
	}

	q := store.LogQuery{
		App:       params.App,
		Container: params.Container,
		HostID:    params.Host,
		Q:         params.Q,
		Stream:    params.Stream,
		Limit:     params.Limit,
	}
	if q.Limit <= 0 || q.Limit > 5000 {
		q.Limit = 500
	}
	if params.Level != "" {
		q.Levels = []string{params.Level}
	}
	if params.Since != "" {
		if ts, err := parseTime(params.Since); err == nil {
			q.SinceMs = ts
		}
	}
	if params.Until != "" {
		if ts, err := parseTime(params.Until); err == nil {
			q.UntilMs = ts
		}
	}

	rows, err := s.st.QueryLogs(ctx, q)
	if err != nil {
		return errorResponse(id, ErrInternal, "query failed: "+err.Error(), nil)
	}

	type logEntry struct {
		ID          int64  `json:"id"`
		TS          int64  `json:"ts"`
		TSMs        int64  `json:"ts_ms"`
		ContainerID string `json:"container_id"`
		Container   string `json:"container"`
		App         string `json:"app"`
		HostID      string `json:"host_id,omitempty"`
		Stream      string `json:"stream"`
		Level       string `json:"level"`
		Line        string `json:"line"`
	}
	entries := make([]logEntry, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, logEntry{
			ID:          r.ID,
			TS:          r.TS,
			TSMs:        r.TSMs,
			ContainerID: r.ContainerID,
			Container:   r.Container,
			App:         r.App,
			HostID:      r.HostID,
			Stream:      r.Stream,
			Level:       r.Level,
			Line:        r.Line,
		})
	}
	return resultResponse(id, map[string]interface{}{"logs": entries})
}

// handleListSnapshots handles the list_snapshots tool.
func (s *Server) handleListSnapshots(ctx context.Context, id *json.RawMessage, args json.RawMessage) Message {
	var params struct {
		App   string `json:"app,omitempty"`
		Host  string `json:"host,omitempty"`
		Limit int    `json:"limit,omitempty"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return errorResponse(id, ErrParams, "invalid arguments", nil)
	}
	if params.Limit <= 0 || params.Limit > 500 {
		params.Limit = 100
	}

	rows, err := s.st.ListSnapshots(ctx, params.App, params.Host, params.Limit)
	if err != nil {
		return errorResponse(id, ErrInternal, "query failed: "+err.Error(), nil)
	}

	type snapEntry struct {
		ID             int64  `json:"id"`
		TS             int64  `json:"ts"`
		HostID         string `json:"host_id,omitempty"`
		DieTS          int64  `json:"die_ts"`
		Kind           string `json:"kind"`
		ContainerID    string `json:"container_id"`
		Container      string `json:"container"`
		App            string `json:"app"`
		Image          string `json:"image"`
		ExitCode       int    `json:"exit_code"`
		OOMKilled      bool   `json:"oom_killed"`
		RestartCount   int    `json:"restart_count"`
		RestartsWindow int    `json:"restarts_window"`
		MemLimit       int64  `json:"mem_limit"`
		UptimeSec      int64  `json:"uptime_sec"`
		Summary        string `json:"summary"`
	}
	entries := make([]snapEntry, 0, len(rows))
	for _, sn := range rows {
		entries = append(entries, snapEntry{
			ID:             sn.ID,
			TS:             sn.TS,
			HostID:         sn.HostID,
			DieTS:          sn.DieTS,
			Kind:           sn.Kind,
			ContainerID:    sn.ContainerID,
			Container:      sn.Container,
			App:            sn.App,
			Image:          sn.Image,
			ExitCode:       sn.ExitCode,
			OOMKilled:      sn.OOMKilled,
			RestartCount:   sn.RestartCount,
			RestartsWindow: sn.RestartsWindow,
			MemLimit:       sn.MemLimit,
			UptimeSec:      sn.UptimeSec,
			Summary:        sn.Summary,
		})
	}
	return resultResponse(id, map[string]interface{}{"snapshots": entries})
}

// handleGetSnapshot handles the get_snapshot tool.
func (s *Server) handleGetSnapshot(ctx context.Context, id *json.RawMessage, args json.RawMessage) Message {
	var params struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return errorResponse(id, ErrParams, "invalid arguments", nil)
	}

	sn, err := s.st.GetSnapshot(ctx, params.ID)
	if err != nil {
		return errorResponse(id, ErrInternal, "snapshot not found: "+err.Error(), nil)
	}

	type logEntry struct {
		ID          int64  `json:"id"`
		TS          int64  `json:"ts"`
		TSMs        int64  `json:"ts_ms"`
		ContainerID string `json:"container_id"`
		Container   string `json:"container"`
		App         string `json:"app"`
		HostID      string `json:"host_id,omitempty"`
		Stream      string `json:"stream"`
		Level       string `json:"level"`
		Line        string `json:"line"`
	}
	type metricEntry struct {
		TS        int64   `json:"ts"`
		HostID    string  `json:"host_id,omitempty"`
		Container string  `json:"container,omitempty"`
		App       string  `json:"app,omitempty"`
		CPU       float64 `json:"cpu"`
		Mem       int64   `json:"mem"`
		MemLimit  int64   `json:"mem_limit"`
		NetRx     int64   `json:"net_rx"`
		NetTx     int64   `json:"net_tx"`
		BlkR      int64   `json:"blk_r"`
		BlkW      int64   `json:"blk_w"`
		Pids      int64   `json:"pids"`
	}
	logs := make([]logEntry, 0, len(sn.Logs))
	for _, r := range sn.Logs {
		logs = append(logs, logEntry{
			ID:          r.ID,
			TS:          r.TS,
			TSMs:        r.TSMs,
			ContainerID: r.ContainerID,
			Container:   r.Container,
			App:         r.App,
			HostID:      r.HostID,
			Stream:      r.Stream,
			Level:       r.Level,
			Line:        r.Line,
		})
	}
	metrics := make([]metricEntry, 0, len(sn.Metrics))
	for _, p := range sn.Metrics {
		metrics = append(metrics, metricEntry{
			TS:        p.TS,
			HostID:    p.HostID,
			Container: p.Container,
			App:       p.App,
			CPU:       p.CPU,
			Mem:       p.Mem,
			MemLimit:  p.MemLimit,
			NetRx:     p.NetRx,
			NetTx:     p.NetTx,
			BlkR:      p.BlkR,
			BlkW:      p.BlkW,
			Pids:      p.Pids,
		})
	}
	return resultResponse(id, map[string]interface{}{
		"id":              sn.ID,
		"ts":              sn.TS,
		"host_id":         sn.HostID,
		"die_ts":          sn.DieTS,
		"kind":            sn.Kind,
		"container_id":    sn.ContainerID,
		"container":       sn.Container,
		"app":             sn.App,
		"image":           sn.Image,
		"exit_code":       sn.ExitCode,
		"oom_killed":      sn.OOMKilled,
		"error":           sn.Error,
		"restart_count":   sn.RestartCount,
		"restarts_window": sn.RestartsWindow,
		"mem_limit":       sn.MemLimit,
		"uptime_sec":      sn.UptimeSec,
		"summary":         sn.Summary,
		"logs":            logs,
		"metrics":         metrics,
	})
}

// handleRunSQL handles the run_sql tool (read-only SELECT only).
func (s *Server) handleRunSQL(ctx context.Context, id *json.RawMessage, args json.RawMessage) Message {
	var params struct {
		SQL   string `json:"sql"`
		Limit int    `json:"limit,omitempty"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return errorResponse(id, ErrParams, "invalid arguments", nil)
	}
	sql := strings.TrimSpace(params.SQL)
	if sql == "" {
		return errorResponse(id, ErrParams, "sql is required", nil)
	}

	res, err := s.st.QuerySQL(ctx, sql, params.Limit)
	if err != nil {
		return errorResponse(id, ErrInternal, "query failed: "+err.Error(), nil)
	}

	// Convert [][]any to []map[string]interface{} for JSON output
	rows := make([]map[string]interface{}, 0, len(res.Rows))
	for _, row := range res.Rows {
		rowMap := make(map[string]interface{}, len(res.Columns))
		for i, col := range res.Columns {
			rowMap[col] = row[i]
		}
		rows = append(rows, rowMap)
	}
	return resultResponse(id, map[string]interface{}{
		"columns":   res.Columns,
		"rows":      rows,
		"truncated": res.Truncated,
		"ms":        res.MS,
	})
}

// parseTime parses an RFC3339 timestamp to unix milliseconds.
func parseTime(s string) (int64, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t, err = time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return 0, err
		}
	}
	return t.UnixMilli(), nil
}
