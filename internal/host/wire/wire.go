// Package wire is the protocol between the host and your process: newline-delimited JSON messages
// on a Unix socket. docs/host-protocol.md describes it.
package wire

import (
	"net"
	"time"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/devicetype"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/host/pluginwire"
)

const (
	// Version is the protocol's version.
	Version = pluginwire.Version
	// RoleAutomations is the one process that runs a home's holds and moments.
	RoleAutomations = "automations"
	// RoleDev runs the home against a simulated one, for oikade dev.
	RoleDev = "dev"
	// RoleReplay runs the home against an event log, for oikade replay. Its report goes to stdout.
	RoleReplay = "replay"
	// RolePlugin is a plugin your home imports. The host starts one process for each.
	RolePlugin = pluginwire.RolePlugin
	// RoleCheck is your binary's check mode: it loads the home, prints its Manifest and exits.
	RoleCheck = "check"
	// KindFlow is the Kind of a RuleInfo for a flow.
	KindFlow = "flow"
	// SocketEnv names the host's socket to the process it starts.
	SocketEnv = "OIKADE_SOCKET"
)

// Type is a message's type.
type Type = pluginwire.Type

// Message types. Hello and Welcome, or Rejected, open a session, and Ping, Pong, Change and
// Discovered flow for as long as it lasts, as do Pause and Resume from the host, and RuleRun,
// RoomReads and Metrics from your process. The rest are requests, each answered by a Reply with its ID. A plugin's
// session has only Hello, Welcome or Rejected, Ping, Pong and Publish, which flows both ways.
const (
	TypeHello           = pluginwire.TypeHello
	TypeWelcome         = pluginwire.TypeWelcome
	TypeRun        Type = "run"
	TypeRejected        = pluginwire.TypeRejected
	TypePing            = pluginwire.TypePing
	TypePong            = pluginwire.TypePong
	TypeChange     Type = "change"
	TypeDiscovered Type = "discovered"
	// TypeSeen is a device reporting, which a value that didn't change doesn't show.
	TypeSeen       Type = "seen"
	TypeClaim      Type = "claim"
	TypeRelease    Type = "release"
	TypeReleaseAll Type = "releaseAll"
	TypeSet        Type = "set"
	TypeSaveHold   Type = "saveHold"
	TypeDeleteHold Type = "deleteHold"
	TypeSaveFire   Type = "saveFire"
	TypeJournal    Type = "journal"
	TypeFinish     Type = "finish"
	TypeReply      Type = "reply"
	TypePause      Type = "pause"
	TypeResume     Type = "resume"
	TypeRuleRun    Type = "ruleRun"
	TypeRoomReads  Type = "roomReads"
	TypeMetrics    Type = "metrics"
	TypePublish         = pluginwire.TypePublish
)

// Manifest is what your binary's check mode prints: enough for the host to judge a version before
// anything stops.
type Manifest struct {
	// Protocol is the Version the binary was built against.
	Protocol int        `json:"protocol"`
	Rules    []RuleInfo `json:"rules"`
	// Plugins names the plugins the binary imports.
	Plugins []string `json:"plugins,omitempty"`
	// Control is every device setting the home declares.
	Control []DeviceControl `json:"control,omitempty"`
}

// DeviceControl is how a home arbitrates one capability of a device.
type DeviceControl struct {
	Device string        `json:"device"`
	Entry  control.Entry `json:"entry"`
}

// RuleInfo is one rule of a home.
type RuleInfo struct {
	Name string `json:"name"`
	// Kind is hold, moment or KindFlow.
	Kind string `json:"kind"`
	// Safety is a rule with Safety priority, or a moment with a Safety set.
	Safety bool `json:"safety,omitempty"`
	// Latch is a hold that stays active until a release.
	Latch bool `json:"latch,omitempty"`
	// Rooms are a Safety rule's room writes limited to device types, so that a device's new type
	// can't take it out of one unnoticed.
	Rooms []RoomOfTypes `json:"rooms,omitempty"`
}

// RoomOfTypes is a room write limited to device types.
type RoomOfTypes struct {
	Room        string            `json:"room"`
	Capability  string            `json:"capability"`
	DeviceTypes []devicetype.Type `json:"deviceTypes"`
}

// Change is one value a change message carries.
type Change struct {
	Capability string `json:"capability"`
	Value      any    `json:"value"`
	Previous   any    `json:"previous,omitempty"`
}

// Device is one device as the host knows it.
type Device struct {
	Name string `json:"name"`
	Room string `json:"room,omitempty"`
	// Arbitrated are the capabilities the host arbitrates, which take claims rather than writes.
	Arbitrated   []string `json:"arbitrated,omitempty"`
	Discovered   bool     `json:"discovered,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	// DeviceTypes are what it is, which a room write can be limited to.
	DeviceTypes []devicetype.Type `json:"deviceTypes,omitempty"`
	// LastSeen is when the device last reported, which says how old its values are.
	LastSeen time.Time `json:"lastSeen,omitzero"`
	// Values are bools and numbers, by capability.
	Values map[string]any `json:"values,omitempty"`
}

// Run is a flow's run that hasn't finished: its journal so far, oldest first.
type Run struct {
	ID      string   `json:"id"`
	Entries [][]byte `json:"entries"`
}

// Message is one line on the socket. Only the fields its Type uses are set.
type Message struct {
	Type Type `json:"type"`
	// ID matches a request to its Reply, and a Ping to its Pong.
	ID uint64 `json:"id,omitempty"`

	// Version and Role are a Hello's.
	Version int    `json:"version,omitempty"`
	Role    string `json:"role,omitempty"`
	// Plugin is a plugin's Hello: which of the binary's plugins this process is.
	Plugin string `json:"plugin,omitempty"`
	// Rules names the process's holds, so the Welcome can return their saved state.
	Rules []string `json:"rules,omitempty"`
	// Info says what the process's rules are, for oikade status and pause.
	Info []RuleInfo `json:"info,omitempty"`
	// Control is every device setting the process's home declares.
	Control []DeviceControl `json:"control,omitempty"`

	// Paused is a Welcome's: rules not to run until a Resume says so.
	Paused []string `json:"paused,omitempty"`
	// Devices, Holds, Runs and Fires are a Welcome's, and Error a Rejected's or a failed Reply's.
	// Runs says how many Run messages follow it: journals can be long, so each goes alone, and
	// Dial gathers them in Unfinished.
	Devices []Device                    `json:"devices,omitempty"`
	Holds   map[string]engine.HoldState `json:"holds,omitempty"`
	Runs    int                         `json:"runs,omitempty"`
	Fires   map[string]time.Time        `json:"fires,omitempty"`
	Error   string                      `json:"error,omitempty"`
	// Unfinished is a Run's run, one per message, and in what Dial returns, all of them.
	Unfinished []Run `json:"unfinished,omitempty"`

	// Device, Capability and Value address a device.
	Device     string `json:"device,omitempty"`
	Capability string `json:"capability,omitempty"`
	Value      any    `json:"value,omitempty"`
	// Changes are a change's: every value one report from Device changed, which your process takes
	// in together.
	Changes []Change `json:"changes,omitempty"`
	// Discovered is a Discovered's device.
	Discovered *Device   `json:"discovered,omitempty"`
	Time       time.Time `json:"time,omitzero"`

	// Claim is a claim's, and Source a release's. Below limits a releaseAll to lower levels.
	Claim  *control.Claim `json:"claim,omitempty"`
	Source string         `json:"source,omitempty"`
	Below  control.Level  `json:"below,omitempty"`
	// Rule and Hold are a saved hold's. Rule and Time are a timer's last accounted fire.
	Rule string            `json:"rule,omitempty"`
	Hold *engine.HoldState `json:"hold,omitempty"`
	// Run and Data are a journal entry's, and Seq its place in the run.
	Run  string `json:"run,omitempty"`
	Data []byte `json:"data,omitempty"`
	Seq  uint64 `json:"seq,omitempty"`

	// Topic, Data and Retain are a Publish's: one message of the oikade/v1 contract. Topic is also
	// a plugin's Welcome's topic root, Declared the devices on record for it, and SetTimeout how
	// long a Set may run before the plugin counts as hung.
	Topic      string                      `json:"topic,omitempty"`
	Retain     bool                        `json:"retain,omitempty"`
	Declared   []contractv1.DeclaredDevice `json:"declared,omitempty"`
	SetTimeout time.Duration               `json:"setTimeout,omitempty"`

	// Arbitrated is a claim or release's Reply: false means the capability isn't.
	Arbitrated bool `json:"arbitrated,omitempty"`

	// Reads are a RoomReads': every room its Rule's hold has read since the process started.
	Reads []engine.RoomRead `json:"reads,omitempty"`
	// Result is a RuleRun's, with Rule and Time.
	Result string `json:"result,omitempty"`
	// Metrics is a Metrics message's: the process's rule metrics in Prometheus's text format.
	Metrics []byte `json:"metrics,omitempty"`
}

// Conn reads and writes Messages. One goroutine may Read while others Write.
type Conn = pluginwire.Conn[Message]

// NewConn wraps a socket.
func NewConn(c net.Conn) *Conn { return pluginwire.NewConn[Message](c) }

// ErrRejected is a session the host refused.
var ErrRejected = pluginwire.ErrRejected
