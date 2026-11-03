package transport

import (
	"capscope/internal/docker"
	"capscope/internal/store"
)

// Messages carried over the agent↔hub stream (Phase 4). The wire encoding is
// length-prefixed gob frames over mutual TLS; capscope.proto is the schema
// contract. These types mirror the proto one-for-one so a future migration to
// generated gRPC bindings changes only the codec, not the data model.

// Hello is the first message a client sends after the TLS handshake. It
// identifies the host (host_id is stable across agent restarts) and its
// labels (Docker Swarm node id, etc.).
type Hello struct {
	HostID  string            `json:"host_id"`
	Labels  map[string]string `json:"labels,omitempty"`
	Version string            `json:"version,omitempty"`
}

// LogBatch is one batch of log rows collected on one host.
type LogBatch struct {
	HostID string         `json:"host_id"`
	Rows   []store.LogRow `json:"rows"`
}

// MetricBatch is one batch of metric samples collected on one host.
type MetricBatch struct {
	HostID  string              `json:"host_id"`
	Samples []store.MetricPoint `json:"samples"`
}

// EventBatch is one batch of Docker lifecycle events collected on one host.
type EventBatch struct {
	HostID string         `json:"host_id"`
	Events []docker.Event `json:"events"`
}

// Container is a snapshot of one container the agent is tracking.
type Container struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	App      string  `json:"app"`
	Replica  string  `json:"replica"`
	Image    string  `json:"image"`
	Started  int64   `json:"started"`
	CPU      float64 `json:"cpu"`
	Mem      int64   `json:"mem"`
	MemLimit int64   `json:"mem_limit"`
	SampleTS int64   `json:"sample_ts"`
}

// ContainerList is a snapshot of all containers the agent is tracking right now.
type ContainerList struct {
	HostID     string      `json:"host_id"`
	Containers []Container `json:"containers"`
}

// Ack is the server's reply to each batch.
type Ack struct {
	HostID string `json:"host_id"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}
