// Package api is the local API: how the oikade command talks to a running host, over a Unix socket
// in the data directory. Each connection carries one JSON request and one JSON response. It has no
// network listener, and it's apart from the socket your process uses.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
)

// SocketName is the local API's socket in the data directory.
const SocketName = "api.sock"

// Request types.
const (
	TypeInfo     = "info"
	TypeDeploy   = "deploy"
	TypeRollback = "rollback"
)

// Request is one call.
type Request struct {
	Type string `json:"type"`
	// Binary is the file a deploy takes. The host copies it.
	Binary string `json:"binary,omitempty"`
	// AllowSafetyRemoval goes ahead even though a Safety rule would go.
	AllowSafetyRemoval bool `json:"allowSafetyRemoval,omitempty"`
}

// Response is its answer.
type Response struct {
	// Error says why it failed.
	Error string `json:"error,omitempty"`
	// Findings are what stopped a version going ahead.
	Findings []string `json:"findings,omitempty"`
	// Version is the version running now.
	Version string `json:"version,omitempty"`
	// Unchanged is a deploy of the version already running.
	Unchanged bool `json:"unchanged,omitempty"`
	// Restored is set when a new version failed once it started, and the last one came back.
	Restored bool `json:"restored,omitempty"`
	// GOOS and GOARCH are what the host runs on, so that a deploy can build for it.
	GOOS   string `json:"goos,omitempty"`
	GOARCH string `json:"goarch,omitempty"`
}

// Call makes a request to the host at socket, and waits for as long as it takes: a deploy watches the
// new version settle.
func Call(ctx context.Context, socket string, req Request) (Response, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return Response{}, fmt.Errorf("reaching the host: %w", err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, fmt.Errorf("sending to the host: %w", err)
	}
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("reading the host's answer: %w", err)
	}
	return resp, nil
}
