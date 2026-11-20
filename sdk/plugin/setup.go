package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sync"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/mqtt"
)

// Setup is a Handler's first-run setup, which oikade setup runs on the host when someone asks. A
// Handler that implements it says so in its status. It asks only for what it can't find out
// itself, keeps what it learns as host secrets, checks the result, and returns a line saying how it
// went, such as "ewelink-lan is set up: 2 devices online". An error is what's still wrong, said so
// that the person knows what to do. Run again, it repeats its checks and changes only what's wrong.
//
// It runs on its own goroutine alongside Serve and Set, so state they share needs a lock. Its ctx
// ends when the person goes, when another setup starts, or when the plugin closes.
type Setup interface {
	Setup(ctx context.Context, p *Plugin, s *Session) (string, error)
}

// ErrSetupEnded is a step of a session that's over: the person went, or another setup started.
var ErrSetupEnded = errors.New("setup ended")

// Session is one run of a Setup: the dialogue with the person who ran oikade setup.
type Session struct {
	p    *Plugin
	name string
	ctx  context.Context

	mu      sync.Mutex
	seq     int
	waiting map[int]chan contractv1.SetupRequest
}

// Say shows the person a line.
func (s *Session) Say(ctx context.Context, line string) error {
	if line == "" {
		return nil
	}
	_, err := s.step(ctx, contractv1.SetupStep{Say: line})
	return err
}

// Ask asks the person a question and returns what they typed.
func (s *Session) Ask(ctx context.Context, prompt string) (string, error) {
	return s.step(ctx, contractv1.SetupStep{Ask: &contractv1.SetupAsk{Prompt: prompt}})
}

// AskSecret asks for something that mustn't show as it's typed, such as a password. Keep it with
// KeepSecret if it's needed again, rather than ask each time.
func (s *Session) AskSecret(ctx context.Context, prompt string) (string, error) {
	return s.step(ctx, contractv1.SetupStep{Ask: &contractv1.SetupAsk{Prompt: prompt, Secret: true}})
}

// Open opens page in the person's browser, such as a sign-in, and returns the query it redirects
// to redirect with, such as an OAuth2 code. redirect is http://127.0.0.1:PORT/PATH, where oikade
// setup listens on the person's machine, so it's also the redirect URL to register with the
// platform.
func (s *Session) Open(ctx context.Context, page, redirect string) (url.Values, error) {
	raw, err := s.step(ctx, contractv1.SetupStep{Open: &contractv1.SetupOpen{URL: page, Redirect: redirect}})
	if err != nil {
		return nil, err
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return nil, fmt.Errorf("reading the redirect: %w", err)
	}
	return q, nil
}

// KeepSecret keeps value as the host's secret name, such as a device's key, and returns once it's on
// disk, so ReadSecret has it straight after, in this process and the plugin's next. A backup has it
// too. It's the plugin's own: nothing shows it to the person setting up, and a deploy keeps it.
func (s *Session) KeepSecret(ctx context.Context, name, value string) error {
	if !contractv1.ValidSecretName(name) {
		return fmt.Errorf("%q isn't a secret's name", name)
	}
	_, err := s.step(ctx, contractv1.SetupStep{Keep: &contractv1.SetupKeep{Name: name, Value: value}})
	return err
}

// step sends one step and, if it needs one, waits for the reply.
func (s *Session) step(ctx context.Context, step contractv1.SetupStep) (string, error) {
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		return "", ErrSetupEnded
	}
	step.Session, step.Seq = s.name, s.seq+1
	if err := step.Validate(); err != nil {
		s.mu.Unlock()
		return "", err
	}
	s.seq = step.Seq
	var reply chan contractv1.SetupRequest
	if step.NeedsReply() {
		reply = make(chan contractv1.SetupRequest, 1)
		s.waiting[step.Seq] = reply
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.waiting, step.Seq)
		s.mu.Unlock()
	}()
	if err := s.p.publishSetup(ctx, step); err != nil {
		// The session ending cancels the publish too, so it says so first.
		if s.ctx.Err() != nil {
			return "", ErrSetupEnded
		}
		return "", err
	}
	if reply == nil {
		return "", nil
	}
	select {
	case r := <-reply:
		if r.Error != "" {
			return "", errors.New(r.Error)
		}
		return r.Value, nil
	case <-ctx.Done():
	case <-s.ctx.Done():
	}
	// Setup's own ctx is the session's, so the session ending says so first.
	if s.ctx.Err() != nil {
		return "", ErrSetupEnded
	}
	return "", ctx.Err()
}

// deliver hands a reply to the step waiting for it. One for no waiting step is dropped.
func (s *Session) deliver(r contractv1.SetupRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w, ok := s.waiting[r.Reply]; ok {
		select {
		case w <- r:
		default:
		}
	}
}

// running is the setup session in progress.
type running struct {
	session *Session
	cancel  context.CancelFunc
}

// receiveSetup runs on the receive goroutine, so a session runs on its own.
func (p *Plugin) receiveSetup(m mqtt.Message) {
	var r contractv1.SetupRequest
	if m.Retained || json.Unmarshal(m.Payload, &r) != nil || r.Session == "" {
		p.log.Warn("Ignored setup request", "topic", m.Topic)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Load() {
		return
	}
	current := p.setup
	switch {
	case r.Start:
		if current != nil {
			current.cancel()
		}
		ctx, cancel := context.WithCancel(p.life)
		s := &Session{p: p, name: r.Session, ctx: ctx, waiting: map[int]chan contractv1.SetupRequest{}}
		p.setup = &running{session: s, cancel: cancel}
		go p.runSetup(s, cancel)
	case current == nil || current.session.name != r.Session:
	case r.End:
		current.cancel()
		p.setup = nil
	case r.Reply > 0:
		current.session.deliver(r)
	}
}

// runSetup runs one session and says how it ended, unless it was ended for it.
func (p *Plugin) runSetup(s *Session, cancel context.CancelFunc) {
	defer cancel()
	done := &contractv1.SetupDone{Error: "this plugin has no setup"}
	if p.onSetup != nil {
		p.log.Info("Started setup", "session", s.name)
		result, err := p.onSetup(s.ctx, p, s)
		switch {
		case s.ctx.Err() != nil:
			p.log.Info("Ended setup", "session", s.name)
			return
		case err != nil:
			p.log.Warn("Could not set up plugin", "session", s.name, "err", err)
			done = &contractv1.SetupDone{Error: err.Error()}
		default:
			p.log.Info("Set up plugin", "session", s.name)
			if result == "" {
				result = "Set up"
			}
			done = &contractv1.SetupDone{Result: result}
		}
	}
	_, _ = s.step(s.ctx, contractv1.SetupStep{Done: done})
	p.mu.Lock()
	if p.setup != nil && p.setup.session == s {
		p.setup = nil
	}
	p.mu.Unlock()
}

// publishSetup publishes a step. Steps aren't retained: they mean something only to the session
// waiting for them.
func (p *Plugin) publishSetup(ctx context.Context, step contractv1.SetupStep) error {
	payload, err := json.Marshal(step)
	if err != nil {
		return err
	}
	if err := p.lock(ctx); err != nil {
		return err
	}
	defer p.unlock()
	return p.client.Publish(ctx, p.root.SetupOut(), payload, false)
}

// SetNeedsSetup says whether the plugin can't serve its devices until someone runs its setup, such
// as one with no credentials yet. It's published with the devices from SetDevices: called before the
// first SetDevices, it returns at once and goes out with it. Only a Handler that implements Setup
// may say it needs one.
func (p *Plugin) SetNeedsSetup(ctx context.Context, needs bool) error {
	if needs && p.onSetup == nil {
		return errors.New("the plugin has no setup to need: its Handler doesn't implement Setup")
	}
	if err := p.lock(ctx); err != nil {
		return err
	}
	defer p.unlock()
	p.mu.Lock()
	changed := p.needsSetup != needs
	p.needsSetup = needs
	devicesSet := p.devices != nil
	p.mu.Unlock()
	if !changed || !devicesSet {
		return nil
	}
	return p.publishDevices(ctx)
}
