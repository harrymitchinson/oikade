// Package wire is the protocol between the host and your process: newline-delimited JSON messages
// on a Unix socket. docs/host-protocol.md describes it.
package wire

import (
	"net"
	"slices"
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
	// RoleAutomations is the one process that runs a home's cases and flows.
	RoleAutomations = "automations"
	// RoleDev runs the home against a simulated one, for oikade dev.
	RoleDev = "dev"
	// RoleReplay runs the home against an event log, for oikade replay. Its report goes to stdout.
	RoleReplay = "replay"
	// RolePlugin is a plugin your home imports. The host starts one process for each.
	RolePlugin = pluginwire.RolePlugin
	// RoleCheck is your binary's check mode: it loads the home, prints its Manifest and exits.
	RoleCheck = "check"
	// KindFlow is the Kind of a RuleInfo for a flow, and KindCase for a decision's case.
	KindFlow = "flow"
	KindCase = "case"
	// SocketEnv names the host's socket to the process it starts.
	SocketEnv = "OIKADE_SOCKET"
)

// Type is a message's type.
type Type = pluginwire.Type

// Message types. Hello and Welcome, or Rejected, open a session, and Ping, Pong, Change and
// Discovered flow for as long as it lasts, as do Pause and Resume from the host, and RuleRun,
// RoomReads, Stopped, WhyNot and Metrics from your process. The rest are requests, each answered by a Reply with its ID. A plugin's
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
	// TypeSaveAverage saves a rolling average a condition reads.
	TypeSaveAverage Type = "saveAverage"
	TypeJournal     Type = "journal"
	TypeFinish      Type = "finish"
	TypeReply       Type = "reply"
	TypePause       Type = "pause"
	TypeResume      Type = "resume"
	TypeRuleRun     Type = "ruleRun"
	TypeRoomReads   Type = "roomReads"
	TypeStopped     Type = "stopped"
	TypeWhyNot      Type = "whyNot"
	TypeMetrics     Type = "metrics"
	TypePublish          = pluginwire.TypePublish
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
	// Warnings are what oikade check warns of, which don't stop a deploy.
	Warnings []string `json:"warnings,omitempty"`
	// Secrets names the secrets the home's code uses, made with oikade.Secret as it was built.
	Secrets []string `json:"secrets,omitempty"`
	// HiddenFromMatter are the plugins whose devices the home keeps off Matter.
	HiddenFromMatter []HiddenPlugin `json:"hiddenFromMatter,omitempty"`
}

// HiddenPlugin is a plugin whose devices a home keeps off Matter, because the Home app has them
// already, except the ones it shows.
type HiddenPlugin struct {
	Plugin string   `json:"plugin"`
	Shown  []string `json:"shown,omitempty"`
}

// Hidden says whether hidden keeps a plugin's device off Matter.
func Hidden(hidden []HiddenPlugin, plugin, device string) bool {
	for _, h := range hidden {
		if h.Plugin == plugin {
			return !slices.Contains(h.Shown, device)
		}
	}
	return false
}

// DeviceControl is how a home arbitrates one capability of a device.
type DeviceControl struct {
	// Device is the device's name, or Room a room's, whose devices of DeviceTypes, or every one with
	// none, take the entry unless they have one of their own.
	Device      string            `json:"device,omitempty"`
	Room        string            `json:"room,omitempty"`
	DeviceTypes []devicetype.Type `json:"deviceTypes,omitempty"`
	Entry       control.Entry     `json:"entry"`
}

// RuleInfo is one rule of a home.
type RuleInfo struct {
	Name string `json:"name"`
	// Kind is KindCase or KindFlow.
	Kind string `json:"kind"`
	// Safety is a case above its decision's override line, or a flow a decision ranks there.
	Safety bool `json:"safety,omitempty"`
	// Latch is a hold that stays active until a release.
	Latch bool `json:"latch,omitempty"`
	// Device, Capability and Case name a case's line in its decision.
	Device     string `json:"device,omitempty"`
	Capability string `json:"capability,omitempty"`
	Case       string `json:"case,omitempty"`
	// Rooms are a Safety rule's room writes limited to device types, so that a device's new type
	// can't take it out of one unnoticed.
	Rooms []RoomOfTypes `json:"rooms,omitempty"`
}

// RoomOfTypes is a room write limited to device types.
type RoomOfTypes struct {
	Room        string            `json:"room"`
	Capability  string            `json:"capability"`
	DeviceTypes []devicetype.Type `json:"deviceTypes"`
	// Except are the devices it leaves out, which have decisions of their own.
	Except []string `json:"except,omitempty"`
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
	// Averages names the process's rolling averages, by engine.Average's Key, so the Welcome can
	// return their saved state, in AverageStates.
	Averages []string `json:"averages,omitempty"`
	// Info says what the process's rules are, for oikade status and pause.
	Info []RuleInfo `json:"info,omitempty"`
	// Control is every device setting the process's home declares.
	Control []DeviceControl `json:"control,omitempty"`
	// MatterName is the Matter bridge's name the process's home declares, empty for the host's
	// default.
	MatterName string `json:"matterName,omitempty"`
	// HiddenFromMatter are the plugins whose devices the process's home keeps off Matter.
	HiddenFromMatter []HiddenPlugin `json:"hiddenFromMatter,omitempty"`

	// Paused is a Welcome's: rules not to run until a Resume says so.
	Paused []string `json:"paused,omitempty"`
	// Devices, Holds, Runs and Fires are a Welcome's, and Error a Rejected's or a failed Reply's.
	// Runs says how many Run messages follow it: journals can be long, so each goes alone, and
	// Dial gathers them in Unfinished.
	Devices []Device                    `json:"devices,omitempty"`
	Holds   map[string]engine.HoldState `json:"holds,omitempty"`
	Runs    int                         `json:"runs,omitempty"`
	Fires   map[string]time.Time        `json:"fires,omitempty"`
	// AverageStates is a Welcome's: the saved averages Averages named.
	AverageStates map[string]engine.AverageState `json:"averageStates,omitempty"`
	Error         string                         `json:"error,omitempty"`
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
	// Average is a saved average's, with Rule its Key.
	Average *engine.AverageState `json:"average,omitempty"`
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
	// Stopped is a Stopped's: why its Rule's hold couldn't tell whether it should be active. Empty
	// means it can again.
	Stopped string `json:"stopped,omitempty"`
	// WhyNot is a WhyNot's: why its Rule's case is inactive, as its conditions say. Empty means it
	// isn't, or can't tell.
	WhyNot string `json:"whyNot,omitempty"`
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

// SettingsFor are the settings a home gives a device: its own, and for a capability it has none of
// its own for, its room's, if it has the capability and is of the room's device types.
func SettingsFor(settings []DeviceControl, name, room string, types []devicetype.Type, capabilities []string) []control.Entry {
	var out []control.Entry
	for _, c := range settings {
		if c.Device == name {
			out = append(out, c.Entry)
		}
	}
	for _, c := range settings {
		if c.Room == "" || c.Room != room || !slices.Contains(capabilities, c.Entry.Capability) ||
			len(c.DeviceTypes) > 0 && !devicetype.Any(types, c.DeviceTypes) ||
			slices.ContainsFunc(out, func(e control.Entry) bool { return e.Capability == c.Entry.Capability }) {
			continue
		}
		out = append(out, c.Entry)
	}
	return out
}
