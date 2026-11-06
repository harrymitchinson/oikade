// Package api is the local API: how the oikade command talks to a running host, over a Unix socket
// in the data directory. Each connection carries one JSON request and one JSON response, or for a
// tail, a stream of them. It has no network listener, and it's apart from the socket your process
// uses. A Transport reaches it, from this machine or another.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// SocketName is the local API's socket in the data directory.
const SocketName = "api.sock"

// Request types.
const (
	TypeInfo     = "info"
	TypeDeploy   = "deploy"
	TypeRollback = "rollback"
	TypeStatus   = "status"
	TypeSet      = "set"
	TypePause    = "pause"
	TypeResume   = "resume"
	TypeTail     = "tail"
)

// Request is one call.
type Request struct {
	Type string `json:"type"`
	// Binary is the file a deploy takes. The host copies it.
	Binary string `json:"binary,omitempty"`
	// AllowSafetyRemoval goes ahead even though a Safety rule would go.
	AllowSafetyRemoval bool `json:"allowSafetyRemoval,omitempty"`
	// Device, Capability and Value are a set's.
	Device     string `json:"device,omitempty"`
	Capability string `json:"capability,omitempty"`
	Value      any    `json:"value,omitempty"`
	// Rule is a pause's or a resume's.
	Rule string `json:"rule,omitempty"`
	// AllowSafety pauses a Safety rule.
	AllowSafety bool `json:"allowSafety,omitempty"`
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
	// NeedsAllowSafety is a pause that AllowSafety would let through.
	NeedsAllowSafety bool `json:"needsAllowSafety,omitempty"`
	// Status is a status's.
	Status *Status `json:"status,omitempty"`
}

// Status is what the host knows now. Its JSON is what oikade status --json prints, so scripts rely on
// it: add to it, and don't rename.
type Status struct {
	// Version is the running version, if known. A deploy under way hides it.
	Version string `json:"version,omitempty"`
	// Process is whether your automations process is connected.
	Process bool     `json:"process"`
	Devices []Device `json:"devices"`
	Rules   []Rule   `json:"rules"`
}

// Device is one device.
type Device struct {
	Name         string       `json:"name"`
	Room         string       `json:"room,omitempty"`
	Online       *bool        `json:"online,omitempty"`
	Capabilities []Capability `json:"capabilities"`
}

// Capability is a device's capability, and who has it if it's arbitrated.
type Capability struct {
	Name  string `json:"name"`
	Value any    `json:"value,omitempty"`
	// Level is the winning level: Default, Normal, Override or Safety. Empty means it isn't arbitrated.
	Level string `json:"level,omitempty"`
	// Wanted is the value that level wants.
	Wanted any `json:"wanted,omitempty"`
	// Claims are its claims, highest first.
	Claims []Claim `json:"claims,omitempty"`
}

// Claim is one claim on a capability.
type Claim struct {
	// Source is who made it, such as Rule/fan, Matter or Command.
	Source  string     `json:"source"`
	Level   string     `json:"level"`
	Value   any        `json:"value,omitempty"`
	Winner  bool       `json:"winner,omitempty"`
	Latched bool       `json:"latched,omitempty"`
	Expires *time.Time `json:"expires,omitempty"`
}

// Rule is one rule of your home.
type Rule struct {
	Name string `json:"name"`
	// Kind is hold, moment or flow. Empty is a paused rule no running code names.
	Kind   string `json:"kind,omitempty"`
	Safety bool   `json:"safety,omitempty"`
	Paused bool   `json:"paused,omitempty"`
}

// Event kinds.
const (
	EventChange  = "change"
	EventRule    = "rule"
	EventProcess = "process"
	EventPause   = "pause"
)

// Event is one line of a tail. Its JSON is what oikade tail --json prints, so scripts rely on it: add
// to it, and don't rename.
type Event struct {
	Time time.Time `json:"time"`
	Type string    `json:"type"`
	// Device, Capability, Value and Previous are a change's.
	Device     string `json:"device,omitempty"`
	Capability string `json:"capability,omitempty"`
	Value      any    `json:"value,omitempty"`
	Previous   any    `json:"previous,omitempty"`
	// Rule is a rule run's or a pause's, and Result how the run ended.
	Rule   string `json:"rule,omitempty"`
	Result string `json:"result,omitempty"`
	// Message says what happened to the process or the pause.
	Message string `json:"message,omitempty"`
}

// Call makes a request over t, and waits for as long as it takes: a deploy watches the new version
// settle.
func Call(ctx context.Context, t Transport, req Request) (Response, error) {
	conn, stop, err := open(ctx, t, req)
	if err != nil {
		return Response{}, err
	}
	defer stop()
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("reading the host's answer: %w", err)
	}
	return resp, nil
}

// Tail streams events to each until ctx ends, each returns an error, or the host goes. It starts
// once the host is listening, which the first Event, with no type, says.
func Tail(ctx context.Context, t Transport, each func(Event) error) error {
	conn, stop, err := open(ctx, t, Request{Type: TypeTail})
	if err != nil {
		return err
	}
	defer stop()
	events := json.NewDecoder(conn)
	for {
		var e Event
		if err := events.Decode(&e); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("lost the host: %w", err)
		}
		if e.Type == "" {
			continue
		}
		if err := each(e); err != nil {
			return err
		}
	}
}

func open(ctx context.Context, t Transport, req Request) (io.Reader, func(), error) {
	conn, err := t.Dial(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("reaching the host: %w", err)
	}
	cancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	stop := func() {
		cancel()
		_ = conn.Close()
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		stop()
		return nil, nil, fmt.Errorf("sending to the host: %w", err)
	}
	return conn, stop, nil
}
