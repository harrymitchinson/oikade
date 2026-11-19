// Package api is the host's API: how oikade talks to a running host. It's HTTP over a
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
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/ident"
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
	// TypeMatterOpen opens the Matter bridge's commissioning window.
	TypeMatterOpen = "matterOpen"
	TypeBackup     = "backup"
	// TypeSetup runs a plugin's setup, streaming its steps, and TypeSetupReply answers one.
	TypeSetup      = "setup"
	TypeSetupReply = "setupReply"

	TypeSecrets      = "secrets"
	TypeSecretSet    = "secretSet"
	TypeSecretRemove = "secretRemove"

	TypeDevices      = "devices"
	TypeDeviceRemove = "deviceRemove"

	TypeClients      = "clients"
	TypeClientAdd    = "clientAdd"
	TypeClientRevoke = "clientRevoke"
	TypeLogin        = "login"
)

// ContentType is what every answer but a stream's is.
const ContentType = "application/json"

// StreamContentType is a tail's and an event log's: lines of JSON.
const StreamContentType = "application/x-ndjson"

// AllowSafetyRemovalParam is a deploy's query parameter for AllowSafetyRemoval.
const AllowSafetyRemovalParam = "allowSafetyRemoval"

// BinaryContentType is a deploy's body.
const BinaryContentType = "application/octet-stream"

// Prefix starts every route, and changes if the API ever breaks.
const Prefix = "/v1"

// Route is where a request type lives. A POST's body is its Request as JSON.
type Route struct {
	Type, Method, Path string
	// Stream answers lines of JSON, not one Response.
	Stream bool
	// Public is a route the network API takes without a token.
	Public bool
}

// Routes are the API's routes.
var Routes = []Route{
	{TypeInfo, http.MethodGet, Prefix + "/info", false, false},
	{TypeDeploy, http.MethodPost, Prefix + "/deploy", false, false},
	{TypeRollback, http.MethodPost, Prefix + "/rollback", false, false},
	{TypeStatus, http.MethodGet, Prefix + "/status", false, false},
	{TypeSet, http.MethodPost, Prefix + "/set", false, false},
	{TypePause, http.MethodPost, Prefix + "/pause", false, false},
	{TypeResume, http.MethodPost, Prefix + "/resume", false, false},
	{TypeTail, http.MethodGet, Prefix + "/tail", true, false},
	{TypeEvents, http.MethodGet, Prefix + "/events", true, false},
	{TypeMatter, http.MethodGet, Prefix + "/matter", false, false},
	{TypeMatterOpen, http.MethodPost, Prefix + "/matter/open", false, false},
	{TypeBackup, http.MethodPost, Prefix + "/backup", false, false},
	{TypeSetup, http.MethodPost, Prefix + "/setup", true, false},
	{TypeSetupReply, http.MethodPost, Prefix + "/setup/reply", false, false},

	{TypeSecrets, http.MethodGet, Prefix + "/secrets", false, false},
	{TypeSecretSet, http.MethodPost, Prefix + "/secrets/set", false, false},
	{TypeSecretRemove, http.MethodPost, Prefix + "/secrets/remove", false, false},

	{TypeDevices, http.MethodGet, Prefix + "/devices", false, false},
	{TypeDeviceRemove, http.MethodPost, Prefix + "/devices/remove", false, false},

	{TypeClients, http.MethodGet, Prefix + "/clients", false, false},
	{TypeClientAdd, http.MethodPost, Prefix + "/clients/add", false, false},
	{TypeClientRevoke, http.MethodPost, Prefix + "/clients/revoke", false, false},
	{TypeLogin, http.MethodPost, Prefix + "/login", false, true},
}

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
	// AllowSafetyRemoval goes ahead even though a Safety rule would go. A deploy sends it as a query
	// parameter, as its body is the binary.
	AllowSafetyRemoval bool `json:"allowSafetyRemoval,omitempty"`
	// Device, Capability and Value are a set's.
	Device     string `json:"device,omitempty"`
	Capability string `json:"capability,omitempty"`
	Value      any    `json:"value,omitempty"`
	// Plugin is a remove's in place of Device: every device of that plugin on record.
	Plugin string `json:"plugin,omitempty"`
	// WindowSeconds is how long a Matter window stays open: 180 to 900, and 0 for 900.
	WindowSeconds int `json:"windowSeconds,omitempty"`
	// Rule is a pause's or a resume's.
	Rule string `json:"rule,omitempty"`
	// AllowSafety pauses a Safety rule, or removes a device a Safety claim holds.
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
	// Session and Seq are a setup reply's: the step it answers. Plugin names the plugin.
	Session string `json:"session,omitempty"`
	Seq     int    `json:"seq,omitempty"`
	// Answer is a setup reply's: what the person typed, or a page's redirect query. It may be a
	// secret, so it's in no answer and no log.
	Answer string `json:"answer,omitempty"`
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
	// HostVersion is oikade-host's own version, an info request's.
	HostVersion string `json:"hostVersion,omitempty"`
	// NeedsAllowSafety is a request that AllowSafety would let through.
	NeedsAllowSafety bool `json:"needsAllowSafety,omitempty"`
	// Status is a status's.
	Status *Status `json:"status,omitempty"`
	// Registry is a devices request's.
	Registry *registry.Snapshot `json:"registry,omitempty"`
	// Warning is something a request that went ahead should tell you, such as a remove or
	// deploy that left a room a hold reads with no devices.
	Warning string `json:"warning,omitempty"`
	// Removed are the devices a remove of a plugin's devices removed.
	Removed []string `json:"removed,omitempty"`
	// Outcome is a set's: the capability once the device answered and nothing wrote it back, with its
	// claims. It's nil if the device hadn't answered in time.
	Outcome *Capability `json:"outcome,omitempty"`
	// Ended are the claims a pause ended, and Kept its Safety claims, which only a release ends, each as
	// the capability's ref, such as device/fan/on.
	Ended []string `json:"ended,omitempty"`
	Kept  []string `json:"kept,omitempty"`
	// UnsetSecrets are a deploy's or a rollback's: the secrets the version now running uses that
	// aren't set.
	UnsetSecrets []string `json:"unsetSecrets,omitempty"`
	// DeviceChanges are a deploy's or a rollback's: what the version's device declarations changed,
	// a line each, and Unseen the names it declares whose hardware hasn't reported.
	DeviceChanges []string `json:"deviceChanges,omitempty"`
	Unseen        []string `json:"unseen,omitempty"`
	// Unimported are the devices on record of plugins the version doesn't import: a deploy's or a
	// rollback's for the version now running, and a devices request's for the one running now.
	Unimported []Unimported `json:"unimported,omitempty"`
	// Problems are a devices request's: what's wrong with a device, by name: offline, as its driver
	// says, and the driver's error, such as "offline: unreachable".
	Problems map[string]string `json:"problems,omitempty"`
	// OutsidePlugins are a devices request's: the plugins that run as processes of their own, which no
	// version imports.
	OutsidePlugins []string `json:"outsidePlugins,omitempty"`
	// SafetyHeld are a devices request's: the plugins' devices on record that a Safety claim holds.
	SafetyHeld []string `json:"safetyHeld,omitempty"`
	// HiddenFromMatter are a devices request's: the devices on record your home keeps off Matter.
	HiddenFromMatter []string `json:"hiddenFromMatter,omitempty"`
	// TakenOffMatter are a deploy's or a rollback's: the devices the version now running took off
	// Matter, which controllers have lost.
	TakenOffMatter []string `json:"takenOffMatter,omitempty"`
	// Matter is a matter request's.
	Matter *Matter `json:"matter,omitempty"`
	// MatterWindow is the window a matter open request opened.
	MatterWindow *MatterWindow `json:"matterWindow,omitempty"`
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

// Unimported is a device on record whose plugin isn't imported, so nothing reports it. It stays on
// the Matter bridge, offline, until oikade devices remove takes it.
type Unimported struct {
	Device string `json:"device"`
	// Ref is the device's ref.
	Ref    string `json:"ref"`
	Plugin string `json:"plugin"`
	// Safety is a device a Safety claim holds, which removing it would end.
	Safety bool `json:"safety,omitempty"`
}

// PluginOf names the plugin that reports a device on record, if one does.
func PluginOf(d registry.Device) (string, bool) {
	return strings.CutPrefix(d.Driver, driver.Plugin+"/")
}

// Bridgeable says whether the Matter bridge shows a device on record, unless its plugin is hidden:
// it's paired, and it's something in Matter's names, which is what the bridge maps.
func Bridgeable(d registry.Device) bool {
	return d.ID != "" && !d.Removed && len(d.DeviceTypes()) > 0
}

// HiddenFromMatter says whether hidden, a home's plugins hidden from Matter, keeps a device the bridge
// would show off it.
func HiddenFromMatter(hidden []wire.HiddenPlugin, d registry.Device) bool {
	plugin, ok := PluginOf(d)
	return ok && Bridgeable(d) && wire.Hidden(hidden, plugin, d.Name)
}

// HiddenDevices names the devices on record that hidden keeps off Matter, by name.
func HiddenDevices(list registry.Snapshot, hidden []wire.HiddenPlugin) []string {
	var out []string
	for _, d := range list.Devices {
		if HiddenFromMatter(hidden, d) {
			out = append(out, d.Name)
		}
	}
	slices.Sort(out)
	return out
}

// UnimportedOf lists the devices on record of plugins that are neither imported nor outside, by
// name. safety says whether a Safety claim holds a device, and may be nil.
func UnimportedOf(list registry.Snapshot, imported, outside []string, safety func(device string) bool) []Unimported {
	var out []Unimported
	for _, d := range list.Devices {
		plugin, ok := PluginOf(d)
		if !ok || d.Removed || slices.Contains(imported, plugin) || slices.Contains(outside, plugin) {
			continue
		}
		u := Unimported{Device: d.Name, Ref: ident.DeviceRef(d.Name).String(), Plugin: plugin}
		if safety != nil {
			u.Safety = safety(d.Name)
		}
		out = append(out, u)
	}
	slices.SortFunc(out, func(a, b Unimported) int { return strings.Compare(a.Device, b.Device) })
	return out
}

// Client is a client of the network API. Its JSON is what oikade client list --json prints, so
// scripts rely on it: add to it, and don't rename.
type Client struct {
	Name string `json:"name"`
	// Ref is its ref, such as client/laptop.
	Ref      string    `json:"ref"`
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
	// Ref is its ref, such as secret/broker.
	Ref string `json:"ref"`
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

// MatterWindow is a commissioning window oikade matter open opened. Its JSON is what oikade matter
// open --json prints, so scripts rely on it: add to it, and don't rename.
type MatterWindow struct {
	// ManualCode and QRCode pair a controller in it, and Passcode is the setup passcode they carry.
	ManualCode string `json:"manualCode"`
	QRCode     string `json:"qrCode"`
	Passcode   uint32 `json:"passcode"`
	// Fresh is codes made for this window, which pair nothing once it closes: the bridge was already
	// paired. Otherwise they're the bridge's own, which oikade matter shows.
	Fresh bool `json:"fresh"`
	// Expires is when the window closes, unless a controller pairs first.
	Expires time.Time `json:"expires"`
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
	// Version is the running version, if known. A deploy under way hides it, and says so in Deploying.
	Version   string `json:"version,omitempty"`
	Deploying bool   `json:"deploying,omitempty"`
	// Process is whether your automations process is connected, and Since when it last connected or
	// went, or the host started.
	Process bool       `json:"process"`
	Since   *time.Time `json:"since,omitempty"`
	Devices []Device   `json:"devices"`
	Rules   []Rule     `json:"rules"`
	// PluginProblems are what plugins say is wrong with them as a whole, such as a login their
	// account refused, by plugin name.
	PluginProblems map[string]string `json:"pluginProblems,omitempty"`
	// NeedsSetup are the plugins that say they can't serve their devices until oikade setup runs.
	NeedsSetup []string `json:"needsSetup,omitempty"`
	// UnsetSecrets are the secrets the running version uses that aren't set.
	UnsetSecrets []string `json:"unsetSecrets,omitempty"`
}

// Device is one device.
type Device struct {
	Name string `json:"name"`
	// Ref is its ref, such as device/desk-plug.
	Ref          string       `json:"ref"`
	Room         string       `json:"room,omitempty"`
	Online       *bool        `json:"online,omitempty"`
	Capabilities []Capability `json:"capabilities"`
	// DeviceTypes are what it is.
	DeviceTypes []devicetype.Type `json:"deviceTypes,omitempty"`
	// PluginNotImported is a device whose plugin the running version doesn't import, so it stays
	// offline until it's imported again or you remove the device.
	PluginNotImported bool `json:"pluginNotImported,omitempty"`
	// Problem is what's wrong with it, as its driver says, such as a missing key.
	Problem string `json:"problem,omitempty"`
	// Declare is the line that would declare it, for a device your home's code doesn't declare.
	Declare string `json:"declare,omitempty"`
}

// Capability is a device's capability, and who has it if it's arbitrated.
type Capability struct {
	Name string `json:"name"`
	// Ref is its ref, such as device/desk-plug/on.
	Ref   string `json:"ref"`
	Value any    `json:"value,omitempty"`
	// Level is the winning level: Default, Normal, Override or Safety. Empty means it isn't arbitrated.
	Level string `json:"level,omitempty"`
	// Wanted is the value that level wants.
	Wanted any `json:"wanted,omitempty"`
	// Case is the decision's case that wins, if one does.
	Case string `json:"case,omitempty"`
	// Claims are its claims, highest first.
	Claims []Claim `json:"claims,omitempty"`
	// Limit is the limit holding the winner back, such as MinimumOff, until LimitUntil.
	Limit      string     `json:"limit,omitempty"`
	LimitUntil *time.Time `json:"limitUntil,omitempty"`
}

// Claim is one claim on a capability.
type Claim struct {
	// Source is the ref of who made it: a case's, such as case/fan/on/humid, a flow's, such as
	// flow/morning, or matter, device or command (oikade set).
	Source string `json:"source"`
	// Case is the decision's case that made it, if one did.
	Case    string     `json:"case,omitempty"`
	Level   string     `json:"level"`
	Value   any        `json:"value,omitempty"`
	Winner  bool       `json:"winner,omitempty"`
	Latched bool       `json:"latched,omitempty"`
	Expires *time.Time `json:"expires,omitempty"`
}

// Rule is one rule of your home.
type Rule struct {
	Name string `json:"name"`
	// Ref is the rule's ref, a case's or a flow's, which oikade pause and resume take.
	Ref string `json:"ref"`
	// Kind is case or flow. Empty is a paused rule no running code names.
	Kind string `json:"kind,omitempty"`
	// Device, Capability and Case name a case: its Name is the three, with a space between.
	Device     string `json:"device,omitempty"`
	Capability string `json:"capability,omitempty"`
	Case       string `json:"case,omitempty"`
	// Safety is a case above its decision's override line, or a flow a decision ranks there.
	Safety bool `json:"safety,omitempty"`
	Paused bool `json:"paused,omitempty"`

	// The rest are a case's, and only a running one's: a paused case has none.

	// Active is whether its start is written. Since is when that was, OnAt when its on-delay ends,
	// and OffAt when its off-delay does.
	Active *bool     `json:"active,omitempty"`
	Since  time.Time `json:"since,omitzero"`
	OnAt   time.Time `json:"onAt,omitzero"`
	OffAt  time.Time `json:"offAt,omitzero"`
	// Latched is a latched hold's, from its start until a release.
	Latched bool `json:"latched,omitempty"`
	// Stopped is why it can't tell whether it should be active: its when couldn't be evaluated, so it
	// keeps its state, or its budget couldn't be planned, so its device stays off. Only a connected
	// process says.
	Stopped string `json:"stopped,omitempty"`
	// WhyNot is why an inactive case's conditions are false, such as "device/pool-water/temperature is 5,
	// not below 3", or a ConditionFunc's label.
	WhyNot string `json:"whyNot,omitempty"`
	// Budget is a budget hold's day.
	Budget *RuleBudget `json:"budget,omitempty"`
}

// RuleBudget is a budget hold's plan for a day, and how it's gone, as it last saved them.
type RuleBudget struct {
	// Date is the planning day, YYYY-MM-DD. It may have ended, if the hold hasn't planned since.
	Date string `json:"date"`
	// TargetMinutes is the day's budget, and PlannedMinutes how much of it the plan places.
	TargetMinutes  int32 `json:"targetMinutes"`
	PlannedMinutes int32 `json:"plannedMinutes"`
	// RunMinutes is how long the target has been on that day, saved at most every 5 minutes.
	RunMinutes int32 `json:"runMinutes"`
}

// Event kinds.
const (
	EventChange  = "change"
	EventRule    = "rule"
	EventProcess = "process"
	EventPause   = "pause"
	// EventWrite is the host writing to a device, for whoever By says: a rule, a Matter controller or
	// oikade set. A change with no write before it was made at the device, by its switch or its
	// maker's app.
	EventWrite = "write"
	// EventRefused is a controller's write that a claim turned away, such as a Home app tap against a
	// Safety hold. Message says which claim holds it, and at what level.
	EventRefused = "refused"
	// EventCall is an outside call from a flow, which a replay answers rather than makes.
	EventCall = "call"
	// EventDeploy is a new version taking over.
	EventDeploy = "deploy"
	// EventAvailability is a device going offline or coming back. Message is offline or online, and
	// Result its driver's problem with it as it went offline, if it has one.
	EventAvailability = "availability"
	// EventPlugin is a plugin's own problem starting or clearing, such as a refused login. Ref is the
	// plugin's, and Message the problem, or empty once it has cleared.
	EventPlugin = "plugin"
	// EventMatter is the Matter bridge's commissioning window opening or closing, or a controller
	// pairing or leaving: Message says which, and Value is the controller's fabric.
	EventMatter = "matter"
	// EventForget is a device forgotten, so its history says why it stops.
	EventForget = "forget"
	// EventRename is a device renamed by a deploy: Device is its name from then on, and Previous the
	// one before.
	EventRename = "rename"
	// eventEnd closes the event log, so that a cut-off fetch isn't taken for a short one.
	eventEnd = "end"
)

// What an availability event's Message says.
const (
	AvailabilityOffline = "offline"
	AvailabilityOnline  = "online"
)

// What a matter event's Message says.
const (
	MatterWindowOpened     = "window opened"
	MatterWindowClosed     = "window closed"
	MatterControllerPaired = "controller paired"
	MatterControllerLeft   = "controller left"
)

// Event is one line of a tail. Its JSON is what oikade tail --json prints, so scripts rely on it: add
// to it, and don't rename.
type Event struct {
	Time time.Time `json:"time,omitzero"`
	Type string    `json:"type"`
	// Device, Capability and Value are a change's, a write's or a refused write's, and Previous a
	// change's. Device is also the device that went offline or came back, was forgotten, or was
	// renamed, with Previous its name before.
	Device     string `json:"device,omitempty"`
	Capability string `json:"capability,omitempty"`
	Value      any    `json:"value,omitempty"`
	Previous   any    `json:"previous,omitempty"`
	// Rule is a rule run's or a pause's, and Result how the run ended, a call's error, or the problem
	// of a device that went offline. Value is also a call's result, and a controller's fabric.
	Rule string `json:"rule,omitempty"`
	// Ref is the ref of what it's about: the capability's, the device's, the rule's or the plugin's,
	// or matter for the bridge.
	Ref    string `json:"ref,omitempty"`
	Result string `json:"result,omitempty"`
	// By is whose write it is, as a ref: the rule whose claim won or whose step set it
	// (case/porch-light/on/dusk, flow/morning), matter for a Matter controller, or command for oikade
	// set. A write has none when no rule's claim decided it, as when a capability goes back to its
	// default, and when it's from before writes said.
	By string `json:"by,omitempty"`
	// Message says what happened to the process, the pause, the device or the bridge, which version a
	// deploy brought, which outside call this is, why a write was refused, or a plugin's problem.
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

// Deploy sends the binary at path to the host, which runs it as your home's new version. It waits
// for as long as that takes, as Call does. progress, if it isn't nil, hears the bytes sent of the
// total as the upload goes, from the goroutine sending them; sent reaches total once it's done.
func Deploy(ctx context.Context, t Transport, path string, allowSafetyRemoval bool,
	progress func(sent, total int64)) (Response, error) {
	f, err := os.Open(path)
	if err != nil {
		return Response{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return Response{}, err
	}
	route, _ := routeOf(TypeDeploy)
	target := route.Path
	if allowSafetyRemoval {
		target += "?" + url.Values{AllowSafetyRemovalParam: {"true"}}.Encode()
	}
	sent := &counting{r: f, total: info.Size(), progress: progress}
	body, err := send(ctx, t, route, target, sent, info.Size(), BinaryContentType)
	if err != nil {
		// net/http may still be reading the body after an answer that came early, so sent is atomic.
		// Only a host lost before it answered may have been cut off mid-upload.
		if lost, ok := errors.AsType[lostError](err); ok && sent.n.Load() < info.Size() {
			// The host deploys nothing it didn't get all of.
			return Response{}, fmt.Errorf("lost the host during the upload, so nothing was deployed: %w", lost.err)
		}
		return Response{}, err
	}
	defer func() { _ = body.Close() }()
	var resp Response
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("reading the host's answer: %w", err)
	}
	return resp, nil
}

// counting counts what's read through it, of total, and tells progress.
type counting struct {
	r        io.Reader
	n        atomic.Int64
	total    int64
	progress func(sent, total int64)
}

func (c *counting) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	sent := c.n.Add(int64(n))
	if c.progress != nil && (n > 0 || c.total == 0) {
		c.progress(sent, c.total)
	}
	return n, err
}

// lostError is a call the host may have had, and may still be doing, but never answered.
type lostError struct{ err error }

func (e lostError) Error() string { return "lost the host before it answered: " + e.err.Error() }

func (e lostError) Unwrap() error { return e.err }

// errUnreachable is a call that never reached the host.
var errUnreachable = errors.New("reaching the host")

// ErrHostStopped is a stream whose host ended it without saying why, as a host does when it stops.
var ErrHostStopped = errors.New("the host stopped")

// Tail streams events to each until ctx ends, each returns an error, or the host goes: ErrHostStopped
// if it stopped, or why it ended the tail if it carries on. It starts once the host is listening,
// which the first Event, with no type, says: then it calls opened, if there is one.
func Tail(ctx context.Context, t Transport, opened func(), each func(Event) error) error {
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
			if errors.Is(err, io.EOF) {
				return ErrHostStopped
			}
			return fmt.Errorf("lost the host: %w", err)
		}
		if e.Type == "" {
			if opened != nil {
				opened()
				opened = nil
			}
			continue
		}
		if e.Type == eventEnd {
			return fmt.Errorf("the host ended the tail: %s", e.Message)
		}
		if err := each(e); err != nil {
			return err
		}
	}
}

// Events reads the host's event log to its end: begin gets its opening, and each gets every event. It
// returns ErrHostStopped if the host stopped before the end, or why the host ended it.
func Events(ctx context.Context, t Transport, begin func(EventLog) error, each func(Event) error) error {
	body, err := open(ctx, t, Request{Type: TypeEvents})
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	events := json.NewDecoder(body)
	var log EventLog
	if err := events.Decode(&log); err != nil {
		return eventLogError(err)
	}
	if err := begin(log); err != nil {
		return err
	}
	for {
		var e Event
		if err := events.Decode(&e); err != nil {
			return eventLogError(err)
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

// eventLogError is an event log that ended before its end line: ErrHostStopped if the host ended it
// between lines, as it does when it stops.
func eventLogError(err error) error {
	if errors.Is(err, io.EOF) {
		return ErrHostStopped
	}
	return fmt.Errorf("reading the event log: %w", err)
}

// LogEnd is the line that closes an event log, where a message says it didn't get to the end, or a
// tail the host cut off while it carries on, where the message says why.
func LogEnd(message string) Event { return Event{Type: eventEnd, Message: message} }

// open makes a call, and returns its answer's body: a Response, or a stream's lines. A refused
// stream comes back as an error.
func open(ctx context.Context, t Transport, req Request) (io.ReadCloser, error) {
	route, ok := routeOf(req.Type)
	if !ok {
		return nil, fmt.Errorf("unknown request %q", req.Type)
	}
	var body io.Reader
	length := int64(0)
	if route.Method == http.MethodPost {
		raw, err := json.Marshal(req)
		if err != nil {
			return nil, fmt.Errorf("sending to the host: %w", err)
		}
		body, length = bytes.NewReader(raw), int64(len(raw))
	}
	return send(ctx, t, route, route.Path, body, length, ContentType)
}

// send makes a call to target, a route's path and query, with body as its content.
func send(ctx context.Context, t Transport, route Route, target string, body io.Reader, length int64,
	contentType string) (io.ReadCloser, error) {
	// The host name is a formality: the transport decides where this goes. http, as the transport's
	// own connection is TLS if it needs to be.
	call, err := http.NewRequestWithContext(ctx, route.Method, "http://oikade"+target, body)
	if err != nil {
		return nil, fmt.Errorf("sending to the host: %w", err)
	}
	if body != nil {
		call.ContentLength = length
		call.Header.Set("Content-Type", contentType)
	}
	if a, ok := t.(authorizer); ok && a.token() != "" {
		call.Header.Set("Authorization", "Bearer "+a.token())
	}
	var dialed atomic.Bool
	client := &http.Client{Transport: &http.Transport{
		// One connection a call: oikade makes few, and a stream holds its own.
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
			return nil, lostError{err}
		}
		return nil, fmt.Errorf("%w: %w", errUnreachable, err)
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
