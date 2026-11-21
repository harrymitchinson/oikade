package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"oikade.dev/contract/v1"
)

// setupBacklog bounds the steps a session holds that nobody has read. A reader that falls this far
// behind loses the session, not the decoder.
const setupBacklog = 64

// ErrNoSetup is a plugin whose status says it has no setup.
var ErrNoSetup = errors.New("the plugin has no setup")

// Setup reports whether the plugin has a setup, and whether it says it needs one run.
func (i *Instance) Setup() (has, needs bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	has = i.upLocked() && i.status.Setup
	return has, has && i.needsSetup
}

// StartSetup starts a setup session and returns its steps, which end with a Done. Answer the steps
// that need a reply with ReplySetup, and call EndSetup to stop early. A plugin runs one session at a
// time, so starting one ends any other. A Done the decoder makes itself, because the plugin can't
// send one, has Seq 0: the plugin went offline, or sent a step Oikade can't use.
func (i *Instance) StartSetup(ctx context.Context, session string) (<-chan contract.SetupStep, error) {
	if session == "" {
		return nil, errors.New("a setup session needs a name")
	}
	i.mu.Lock()
	switch {
	case !i.upLocked():
		i.mu.Unlock()
		return nil, errors.New("the plugin isn't online")
	case !i.status.Setup:
		i.mu.Unlock()
		return nil, ErrNoSetup
	}
	if i.sessions == nil {
		i.sessions = map[string]chan contract.SetupStep{}
	}
	if _, ok := i.sessions[session]; ok {
		i.mu.Unlock()
		return nil, fmt.Errorf("setup session %s is already running", session)
	}
	steps := make(chan contract.SetupStep, setupBacklog)
	i.sessions[session] = steps
	i.mu.Unlock()
	if err := i.publishSetup(ctx, contract.SetupRequest{Session: session, Start: true}); err != nil {
		i.mu.Lock()
		i.dropSessionLocked(session)
		i.mu.Unlock()
		return nil, err
	}
	// The plugin ends any other session itself, now that it has this one's start.
	i.mu.Lock()
	for name := range i.sessions {
		if name != session {
			i.endSessionLocked(name, "another setup started")
		}
	}
	i.mu.Unlock()
	return steps, nil
}

// ReplySetup answers step seq of a session: with value, or with why it failed.
func (i *Instance) ReplySetup(ctx context.Context, session string, seq int, value, problem string) error {
	return i.publishSetup(ctx, contract.SetupRequest{Session: session, Reply: seq, Value: value, Error: problem})
}

// EndSetup stops a session. Its steps end without a Done.
func (i *Instance) EndSetup(ctx context.Context, session string) error {
	i.mu.Lock()
	_, running := i.sessions[session]
	i.dropSessionLocked(session)
	i.mu.Unlock()
	if !running {
		return nil
	}
	return i.publishSetup(ctx, contract.SetupRequest{Session: session, End: true})
}

func (i *Instance) publishSetup(ctx context.Context, r contract.SetupRequest) error {
	payload, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return i.Bus.Publish(ctx, i.Root.SetupIn(), payload, false)
}

func (i *Instance) handleSetup(topic string, payload []byte, retained bool) {
	var step contract.SetupStep
	if err := json.Unmarshal(payload, &step); err != nil {
		i.mu.Lock()
		i.rejectLocked(ReasonMalformed, "", topic)
		i.mu.Unlock()
		return
	}
	// A session the decoder gives up on is ended for the plugin too, which would otherwise wait
	// for a reply nobody will send. Published after unlocking: a bus may deliver it straight back.
	if i.handleSetupStep(topic, step, retained) {
		_ = i.publishSetup(context.Background(), contract.SetupRequest{Session: step.Session, End: true})
	}
}

// handleSetupStep hands a step to its session, and reports whether it ended the session for a step
// it couldn't hand on.
func (i *Instance) handleSetupStep(topic string, step contract.SetupStep, retained bool) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	steps, ok := i.sessions[step.Session]
	if retained || !ok {
		// Another session's, or one that's over: nobody is waiting for it. A retained one is the
		// plugin's mistake, but it can't be anyone's step now.
		return false
	}
	if err := step.Validate(); err != nil {
		i.rejectLocked(ReasonMalformed, "", topic)
		i.endSessionLocked(step.Session, "the plugin sent a setup step Oikade can't use: "+err.Error())
		return true
	}
	select {
	case steps <- step:
	default:
		i.endSessionLocked(step.Session, "nobody read the plugin's setup steps")
		return true
	}
	if step.Done != nil {
		i.dropSessionLocked(step.Session)
	}
	return false
}

// endSessionLocked ends a session with a Done of its own, for a plugin that can't send one.
func (i *Instance) endSessionLocked(session, problem string) {
	steps, ok := i.sessions[session]
	if !ok {
		return
	}
	select {
	case steps <- contract.SetupStep{Session: session, Done: &contract.SetupDone{Error: problem}}:
	default:
	}
	i.dropSessionLocked(session)
}

func (i *Instance) dropSessionLocked(session string) {
	if steps, ok := i.sessions[session]; ok {
		close(steps)
		delete(i.sessions, session)
	}
}

// endSessionsLocked ends every session, as when the plugin goes offline.
func (i *Instance) endSessionsLocked(problem string) {
	for name := range i.sessions {
		i.endSessionLocked(name, problem)
	}
}
