// Package api is the host's API: how the oikade command talks to a running host. It's HTTP over a
// Unix socket in the data directory, apart from the socket your process uses, with its routes in
// docs/host-api.md. A Transport reaches it, from this machine or another.
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/harrymitchinson/oikade/internal/registry"
)

// SocketName is the API's socket in the data directory.
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
	TypeEvents   = "events"
	TypeMatter   = "matter"
	TypeBackup   = "backup"

	TypeSecrets      = "secrets"
	TypeSecretSet    = "secretSet"
	TypeSecretRemove = "secretRemove"

	TypeDevices       = "devices"
	TypeDeviceName    = "deviceName"
	TypeDeviceRoom    = "deviceRoom"
	TypeDeviceReserve = "deviceReserve"
	TypeDeviceRemove  = "deviceRemove"
	TypeDeviceReplace = "deviceReplace"
	TypeDeviceRestore = "deviceRestore"

	TypeClients      = "clients"
	TypeClientAdd    = "clientAdd"
	TypeClientRevoke = "clientRevoke"
	TypeLogin        = "login"
)

// ContentType is what every answer but a stream's is.
const ContentType = "application/json"

// StreamContentType is a tail's and an event log's: lines of JSON.
const StreamContentType = "application/x-ndjson"

// Prefix starts every route, and changes if the API ever breaks.
const Prefix = "/v1"

// Route is where a request type lives. A POST's body is its Request as JSON.
type Route struct {
	Type, Method, Path string
	// Stream answers lines of JSON, not one Response.
	Stream bool
}

// Routes are the API's routes.
var Routes = []Route{
	{TypeInfo, http.MethodGet, Prefix + "/info", false},
	{TypeDeploy, http.MethodPost, Prefix + "/deploy", false},
	{TypeRollback, http.MethodPost, Prefix + "/rollback", false},
	{TypeStatus, http.MethodGet, Prefix + "/status", false},
	{TypeSet, http.MethodPost, Prefix + "/set", false},
	{TypePause, http.MethodPost, Prefix + "/pause", false},
	{TypeResume, http.MethodPost, Prefix + "/resume", false},
	{TypeTail, http.MethodGet, Prefix + "/tail", true},
	{TypeEvents, http.MethodGet, Prefix + "/events", true},
	{TypeMatter, http.MethodGet, Prefix + "/matter", false},
	{TypeBackup, http.MethodPost, Prefix + "/backup", false},

	{TypeSecrets, http.MethodGet, Prefix + "/secrets", false},
	{TypeSecretSet, http.MethodPost, Prefix + "/secrets/set", false},
	{TypeSecretRemove, http.MethodPost, Prefix + "/secrets/remove", false},

	{TypeDevices, http.MethodGet, Prefix + "/devices", false},
	{TypeDeviceName, http.MethodPost, Prefix + "/devices/name", false},
	{TypeDeviceRoom, http.MethodPost, Prefix + "/devices/room", false},
	{TypeDeviceReserve, http.MethodPost, Prefix + "/devices/reserve", false},
	{TypeDeviceRemove, http.MethodPost, Prefix + "/devices/remove", false},
	{TypeDeviceReplace, http.MethodPost, Prefix + "/devices/replace", false},
	{TypeDeviceRestore, http.MethodPost, Prefix + "/devices/restore", false},

	{TypeClients, http.MethodGet, Prefix + "/clients", false},
	{TypeClientAdd, http.MethodPost, Prefix + "/clients/add", false},
	{TypeClientRevoke, http.MethodPost, Prefix + "/clients/revoke", false},
	{TypeLogin, http.MethodPost, Prefix + "/login", false},
}

// LoginPath is the one route the network API takes without a token.
const LoginPath = Prefix + "/login"

func routeOf(requestType string) (Route, bool) {
	for _, r := range Routes {
		if r.Type == requestType {
			return r, true
		}
	}
	return Route{}, false
}

// Request is one call.
type Request struct {
	// Type says which call. A route sets it, so it isn't in the body.
	Type string `json:"-"`
	// Binary is the file a deploy takes. The host copies it.
	Binary string `json:"binary,omitempty"`
	// AllowSafetyRemoval goes ahead even though a Safety rule would go.
	AllowSafetyRemoval bool `json:"allowSafetyRemoval,omitempty"`
	// Device, Capability and Value are a set's.
	Device     string `json:"device,omitempty"`
	Capability string `json:"capability,omitempty"`
	Value      any    `json:"value,omitempty"`
	// To is a device's new name, or the device whose hardware a replace takes.
	To string `json:"to,omitempty"`
	// Room is a device's room.
	Room string `json:"room,omitempty"`
	// Registry is what a restore reads back.
	Registry *registry.Snapshot `json:"registry,omitempty"`
	// Force restores a file that disagrees with the registry.
	Force bool `json:"force,omitempty"`
	// Rule is a pause's or a resume's.
	Rule string `json:"rule,omitempty"`
	// AllowSafety pauses a Safety rule, removes a device a Safety claim holds, or changes a device's
	// name while a Safety rule runs.
	AllowSafety bool `json:"allowSafety,omitempty"`
	// Secret names a secret to set or remove.
	Secret string `json:"secret,omitempty"`
	// SecretValue is what to set it to. It's in no answer and no log.
	SecretValue string `json:"secretValue,omitempty"`
	// Client names a client of the network API to add or revoke.
	Client string `json:"client,omitempty"`
	// Token asks client add for a token rather than a code to log in with.
	Token bool `json:"token,omitempty"`
	// Code is what a login trades for a token.
	Code string `json:"code,omitempty"`
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
	// NeedsAllowSafety is a request that AllowSafety would let through.
	NeedsAllowSafety bool `json:"needsAllowSafety,omitempty"`
	// NeedsForce is a restore that Force would let through.
	NeedsForce bool `json:"needsForce,omitempty"`
	// Status is a status's.
	Status *Status `json:"status,omitempty"`
	// Registry is a devices request's.
	Registry *registry.Snapshot `json:"registry,omitempty"`
	// Moved are the devices a restore had to give a new endpoint or name.
	Moved []string `json:"moved,omitempty"`
	// Matter is a matter request's.
	Matter *Matter `json:"matter,omitempty"`
	// Backup is the file a backup wrote, on the host.
	Backup string `json:"backup,omitempty"`
	// Secrets are the secrets the host has, and never their values.
	Secrets []Secret `json:"secrets,omitempty"`
	// Clients are the network API's clients, and never their tokens.
	Clients []Client `json:"clients,omitempty"`
	// Token is a new client's, shown only once.
	Token string `json:"token,omitempty"`
	// Code is what a new client logs in with, once, before Expires.
	Code    string    `json:"code,omitempty"`
	Expires time.Time `json:"expires,omitzero"`
	// Address is where the host thinks it can be reached. A client may know better.
	Address string `json:"address,omitempty"`
	// Client is who a login made.
	Client string `json:"client,omitempty"`
	// Pin is the network API's public key, as a client checks it. It's empty while the host has no
	// network listener.
	Pin string `json:"pin,omitempty"`
}

// Client is a client of the network API. Its JSON is what oikade client list --json prints, so
// scripts rely on it: add to it, and don't rename.
type Client struct {
	Name     string    `json:"name"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"lastUsed,omitzero"`
}

// Pin is how a client recognises the host: the SHA-256 of its certificate's public key, from
// SubjectPublicKeyInfo.
func Pin(publicKeyInfo []byte) string {
	sum := sha256.Sum256(publicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Secret is what the host says about a credential. Its JSON is what oikade secret list --json prints,
// so scripts rely on it: add to it, and don't rename.
type Secret struct {
	Name string `json:"name"`
	// Changed is when its value last changed.
	Changed time.Time `json:"changed"`
}

// Matter is the host's Matter bridge. Its JSON is what oikade matter --json prints, so scripts rely on
// it: add to it, and don't rename.
type Matter struct {
	// Serving is whether the bridge is up. It's down if its port is taken, mDNS wouldn't start, or
	// what it keeps in the store can't be read.
	Serving bool `json:"serving"`
	// Problem is why it's down, or why it last went down while it's trying again.
	Problem string `json:"problem,omitempty"`
	// Port is its UDP port.
	Port uint16 `json:"port"`
	// ManualCode and QRCode pair it. They stay the same for as long as the data directory does, and
	// are empty if the bridge's identity can't be read.
	ManualCode string `json:"manualCode"`
	QRCode     string `json:"qrCode"`
	// Passcode is the setup passcode they carry.
	Passcode uint32 `json:"passcode"`
	// Window is whether a controller can commission it now.
	Window bool `json:"window"`
	// Fabrics are the controllers it's paired with.
	Fabrics []Fabric `json:"fabrics"`
}

// Fabric is one controller's fabric.
type Fabric struct {
	Index    uint8  `json:"index"`
	VendorID uint16 `json:"vendorID"`
	Label    string `json:"label,omitempty"`
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
	// EventWrite is your automations writing to a device. A person's write isn't one: oikade set and
	// the Home app show up as the change they make.
	EventWrite = "write"
	// EventCall is an outside call from a flow, which a replay answers rather than makes.
	EventCall = "call"
	// EventDeploy is a new version taking over.
	EventDeploy = "deploy"
	// eventEnd closes the event log, so that a cut-off fetch isn't taken for a short one.
	eventEnd = "end"
)

// Event is one line of a tail. Its JSON is what oikade tail --json prints, so scripts rely on it: add
// to it, and don't rename.
type Event struct {
	Time time.Time `json:"time,omitzero"`
	Type string    `json:"type"`
	// Device, Capability and Value are a change's or a write's, and Previous a change's.
	Device     string `json:"device,omitempty"`
	Capability string `json:"capability,omitempty"`
	Value      any    `json:"value,omitempty"`
	Previous   any    `json:"previous,omitempty"`
	// Rule is a rule run's or a pause's, and Result how the run ended, or a call's error. Value is
	// also a call's result.
	Rule   string `json:"rule,omitempty"`
	Result string `json:"result,omitempty"`
	// Message says what happened to the process or the pause, which version a deploy brought, or
	// which outside call this is.
	Message string `json:"message,omitempty"`
}

// EventLog opens the host's event log, which comes as lines of JSON: this, then an Event for each
// thing it holds, oldest first.
type EventLog struct {
	// Now is when the host read it.
	Now time.Time `json:"now"`
	// Zone is the IANA name of the time zone your automations run in on the host, and Offset that
	// zone's seconds east of UTC now, for a host that can't name it.
	Zone   string `json:"zone,omitempty"`
	Offset int    `json:"offset"`
	// Baseline is each device's values before the log's first event, by device and capability.
	Baseline map[string]map[string]any `json:"baseline,omitempty"`
}

// Call makes a request over t, and waits for as long as it takes: a deploy watches the new version
// settle.
func Call(ctx context.Context, t Transport, req Request) (Response, error) {
	body, err := open(ctx, t, req)
	if err != nil {
		return Response{}, err
	}
	defer func() { _ = body.Close() }()
	var resp Response
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("reading the host's answer: %w", err)
	}
	return resp, nil
}

// Tail streams events to each until ctx ends, each returns an error, or the host goes. It starts
// once the host is listening, which the first Event, with no type, says.
func Tail(ctx context.Context, t Transport, each func(Event) error) error {
	body, err := open(ctx, t, Request{Type: TypeTail})
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer func() { _ = body.Close() }()
	events := json.NewDecoder(body)
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

// Events reads the host's event log to its end: begin gets its opening, and each gets every event.
func Events(ctx context.Context, t Transport, begin func(EventLog) error, each func(Event) error) error {
	body, err := open(ctx, t, Request{Type: TypeEvents})
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	events := json.NewDecoder(body)
	var log EventLog
	if err := events.Decode(&log); err != nil {
		return fmt.Errorf("reading the event log: %w", err)
	}
	if err := begin(log); err != nil {
		return err
	}
	for {
		var e Event
		if err := events.Decode(&e); err != nil {
			return fmt.Errorf("reading the event log: %w", err)
		}
		if e.Type == eventEnd {
			if e.Message != "" {
				return fmt.Errorf("reading the event log: %s", e.Message)
			}
			return nil
		}
		if err := each(e); err != nil {
			return err
		}
	}
}

// LogEnd is the line that closes an event log. A message says it didn't get to the end.
func LogEnd(message string) Event { return Event{Type: eventEnd, Message: message} }

// open makes a call, and returns its answer's body: a Response, or a stream's lines. A refused
// stream comes back as an error.
func open(ctx context.Context, t Transport, req Request) (io.ReadCloser, error) {
	route, ok := routeOf(req.Type)
	if !ok {
		return nil, fmt.Errorf("unknown request %q", req.Type)
	}
	var body io.Reader
	if route.Method == http.MethodPost {
		raw, err := json.Marshal(req)
		if err != nil {
			return nil, fmt.Errorf("sending to the host: %w", err)
		}
		body = bytes.NewReader(raw)
	}
	// The host name is a formality: the transport decides where this goes.
	// http, as the transport's own connection is TLS if it needs to be.
	call, err := http.NewRequestWithContext(ctx, route.Method, "http://oikade"+route.Path, body)
	if err != nil {
		return nil, fmt.Errorf("sending to the host: %w", err)
	}
	if a, ok := t.(authorizer); ok && a.token() != "" {
		call.Header.Set("Authorization", "Bearer "+a.token())
	}
	var dialed atomic.Bool
	client := &http.Client{Transport: &http.Transport{
		// One connection a call, as a connection over SSH is a process to end.
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			conn, err := t.Dial(ctx)
			dialed.Store(err == nil)
			return conn, err
		},
	}}
	answer, err := client.Do(call)
	if err != nil {
		// Not the URL, which is made up.
		if u, ok := errors.AsType[*url.Error](err); ok {
			err = u.Err
		}
		if dialed.Load() {
			// The host may have the call, and may still be doing it.
			return nil, fmt.Errorf("lost the host before it answered: %w", err)
		}
		return nil, fmt.Errorf("reaching the host: %w", err)
	}
	if answer.StatusCode < http.StatusBadRequest {
		return answer.Body, nil
	}
	if answer.Header.Get("Content-Type") == ContentType && !route.Stream {
		return answer.Body, nil // Call reads why from the Response.
	}
	defer func() { _ = answer.Body.Close() }()
	var refused Response
	if json.NewDecoder(answer.Body).Decode(&refused) == nil && refused.Error != "" {
		return nil, errors.New(refused.Error)
	}
	return nil, fmt.Errorf("the host answered %s", answer.Status)
}
