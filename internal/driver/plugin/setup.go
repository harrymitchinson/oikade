package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
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
// time, so starting one ends any other.
func (i *Instance) StartSetup(ctx context.Context, session string) (<-chan contractv1.SetupStep, error) {
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
		i.sessions = map[string]chan contractv1.SetupStep{}
	}
	if _, ok := i.sessions[session]; ok {
		i.mu.Unlock()
		return nil, fmt.Errorf("setup session %s is already running", session)
	}
	// The plugin ends any other session itself; these won't hear from it again.
	for name := range i.sessions {
		i.endSessionLocked(name, "another setup started")
	}
	steps := make(chan contractv1.SetupStep, setupBacklog)
	i.sessions[session] = steps
	i.mu.Unlock()
	if err := i.publishSetup(ctx, contractv1.SetupRequest{Session: session, Start: true}); err != nil {
		i.mu.Lock()
		i.dropSessionLocked(session)
		i.mu.Unlock()
		return nil, err
	}
	return steps, nil
}

// ReplySetup answers step seq of a session: with value, or with why it failed.
func (i *Instance) ReplySetup(ctx context.Context, session string, seq int, value, problem string) error {
	return i.publishSetup(ctx, contractv1.SetupRequest{Session: session, Reply: seq, Value: value, Error: problem})
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
	return i.publishSetup(ctx, contractv1.SetupRequest{Session: session, End: true})
}

func (i *Instance) publishSetup(ctx context.Context, r contractv1.SetupRequest) error {
	payload, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return i.Bus.Publish(ctx, i.Root.SetupIn(), payload, false)
}

func (i *Instance) handleSetup(topic string, payload []byte, retained bool) {
	var step contractv1.SetupStep
	err := json.Unmarshal(payload, &step)
	i.mu.Lock()
	defer i.mu.Unlock()
	if err != nil {
		i.rejectLocked(ReasonMalformed, "", topic)
		return
	}
	steps, ok := i.sessions[step.Session]
	if retained || !ok {
		// Another session's, or one that's over: nobody is waiting for it. A retained one is the
		// plugin's mistake, but it can't be anyone's step now.
		return
	}
	if err := step.Validate(); err != nil {
		i.rejectLocked(ReasonMalformed, "", topic)
		i.endSessionLocked(step.Session, "the plugin sent a setup step Oikade can't use: "+err.Error())
		return
	}
	select {
	case steps <- step:
	default:
		i.endSessionLocked(step.Session, "nobody read the plugin's setup steps")
		return
	}
	if step.Done != nil {
		i.dropSessionLocked(step.Session)
	}
}

// endSessionLocked ends a session with a Done of its own, for a plugin that can't send one.
func (i *Instance) endSessionLocked(session, problem string) {
	steps, ok := i.sessions[session]
	if !ok {
		return
	}
	select {
	case steps <- contractv1.SetupStep{Session: session, Done: &contractv1.SetupDone{Error: problem}}:
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
