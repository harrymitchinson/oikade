package host

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	pluginDriver "github.com/harrymitchinson/oikade/internal/driver/plugin"
	"github.com/harrymitchinson/oikade/internal/host/api"
)

// setupIdle is how long a setup waits for its plugin's next step, or for the person's answer, before
// the host ends it. A sign-in in a browser takes a few minutes at most.
const setupIdle = 10 * time.Minute

// setupRuns are the setups running, one a plugin at most, so that a reply finds its session and
// answers only a step that's waiting.
type setupRuns struct {
	// starting is held from starting a session until it's recorded, so that of two setups of a
	// plugin started together, the one recorded is the one the plugin runs.
	starting sync.Mutex
	mu       sync.Mutex
	runs     map[string]*setupRun // plugin -> run
}

type setupRun struct {
	session string
	inst    *pluginDriver.Instance
	// waiting are the steps that need the person's answer, which a reply may answer once.
	waiting map[int]bool
	// replied wakes the stream, so that an answer counts as activity.
	replied chan struct{}
}

func (r *setupRuns) add(plugin string, run *setupRun) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runs == nil {
		r.runs = map[string]*setupRun{}
	}
	r.runs[plugin] = run
}

func (r *setupRuns) remove(plugin string, run *setupRun) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runs[plugin] == run {
		delete(r.runs, plugin)
	}
}

func (r *setupRuns) wait(run *setupRun, seq int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run.waiting[seq] = true
}

// take finds the run a reply is for and marks its step answered, or says why it can't.
func (r *setupRuns) take(plugin, session string, seq int) (*setupRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run := r.runs[plugin]
	switch {
	case run == nil || run.session != session:
		return nil, fmt.Errorf("no setup of %s is running as %s", plugin, session)
	case !run.waiting[seq]:
		return nil, fmt.Errorf("step %d of the setup of %s isn't waiting for an answer", seq, plugin)
	}
	delete(run.waiting, seq)
	return run, nil
}

// setupOf is a plugin's decoder, if the host runs or follows it.
func (p *plugins) setupOf(name string) (*pluginDriver.Instance, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.running[name]
	if !ok || h.driver == nil {
		return nil, false
	}
	return h.driver, true
}

// needingSetup names the plugins that say they need their setup run.
func (p *plugins) needingSetup() []string {
	p.mu.Lock()
	running := make([]*hosted, 0, len(p.running))
	for _, h := range p.running {
		running = append(running, h)
	}
	p.mu.Unlock()
	var out []string
	for _, h := range running {
		if _, needs := h.driver.Setup(); needs {
			out = append(out, h.name)
		}
	}
	slices.Sort(out)
	return out
}

// setup runs a plugin's setup for the caller, streaming its steps, and keeps the secrets it asks the
// host to keep. It ends when the plugin is done, the caller goes, nothing happens for setupIdle, or the
// host stops. Secrets and answers are never logged, and a secret's value never goes to the caller.
func (s *apiServer) setup(w http.ResponseWriter, r *http.Request) {
	var req api.Request
	if err := s.read(w, r, &req); err != nil {
		respond(s.log, w, http.StatusBadRequest, api.Response{Error: fmt.Sprintf("reading the request: %v", err)})
		return
	}
	client := callerOf(r.Context()).name
	s.log.Info("Received API request", "type", api.TypeSetup, "client", client, "plugin", req.Plugin)
	var inst *pluginDriver.Instance
	ok := false
	if s.srv.plugins != nil {
		inst, ok = s.srv.plugins.setupOf(req.Plugin)
	}
	if !ok {
		respond(s.log, w, http.StatusUnprocessableEntity, api.Response{
			Error: fmt.Sprintf("there's no plugin %s: oikade devices lists the plugins", req.Plugin),
		})
		return
	}
	session := rand.Text()
	s.setups.starting.Lock()
	steps, err := inst.StartSetup(r.Context(), session)
	run := &setupRun{session: session, inst: inst, waiting: map[int]bool{}, replied: make(chan struct{}, 1)}
	if err == nil {
		s.setups.add(req.Plugin, run)
	}
	s.setups.starting.Unlock()
	if err != nil {
		problem := fmt.Sprintf("could not start the setup of %s: %v", req.Plugin, err)
		if errors.Is(err, pluginDriver.ErrNoSetup) {
			problem = req.Plugin + " has no setup: its README says how to set it up"
		}
		respond(s.log, w, http.StatusUnprocessableEntity, api.Response{Error: problem})
		return
	}
	log := s.log.With("plugin", req.Plugin, "session", session)
	log.Info("Started setup", "client", client)
	defer func() {
		s.setups.remove(req.Plugin, run)
		// Nothing if it's over already. The caller may have gone, so not its context.
		_ = inst.EndSetup(context.WithoutCancel(r.Context()), session)
	}()

	out := newLines(w)
	defer out.end()
	write := func(step api.SetupStep) bool { return out.write(step) && out.flush() }
	if !write(api.SetupStep{Session: session}) {
		return
	}
	idle := time.NewTimer(setupIdle)
	defer idle.Stop()
	for {
		select {
		case step, ok := <-steps:
			if !ok {
				// Ended without a done: another setup of the plugin started, or this one fell behind.
				write(api.SetupStep{Session: session, Done: &contractv1.SetupDone{
					Error: "the setup ended: another setup of " + req.Plugin + " started, or this one fell behind",
				}})
				return
			}
			idle.Reset(setupIdle)
			line, ok := s.setupStep(log, run, step)
			if ok && !write(line) {
				return
			}
			if step.Done != nil {
				if step.Done.Error != "" {
					log.Info("Ended setup", "problem", step.Done.Error)
				} else {
					log.Info("Ended setup")
				}
				return
			}
		case <-run.replied:
			idle.Reset(setupIdle)
		case <-idle.C:
			log.Info("Ended setup", "problem", "timed out")
			write(api.SetupStep{Session: session, Done: &contractv1.SetupDone{
				Error: fmt.Sprintf("nothing happened for %s, so the host ended the setup", setupIdle),
			}})
			return
		case <-r.Context().Done():
			log.Info("Ended setup", "problem", "the caller went")
			return
		case <-s.ctx.Done():
			return
		}
	}
}

// setupStep acts on a plugin's step and says what to pass on, if anything. A keep is the host's to
// do, and only its name goes on.
func (s *apiServer) setupStep(log *slog.Logger, run *setupRun, step contractv1.SetupStep) (api.SetupStep, bool) {
	line := api.SetupStep{Session: step.Session, Seq: step.Seq, Say: step.Say, Ask: step.Ask, Open: step.Open, Done: step.Done}
	switch {
	case step.Keep != nil:
		problem := ""
		if err := s.secrets.set(step.Keep.Name, step.Keep.Value); err != nil {
			problem = err.Error()
			log.Warn("Could not keep secret", "name", step.Keep.Name, "err", err)
		} else {
			log.Info("Changed secret", "name", step.Keep.Name)
		}
		// Not the caller's context: the secret is kept whether it's still there or not.
		if err := run.inst.ReplySetup(s.ctx, run.session, step.Seq, "", problem); err != nil {
			log.Warn("Could not reply to setup", "err", err)
		}
		if problem != "" {
			return api.SetupStep{}, false // The plugin says what it makes of it.
		}
		return api.SetupStep{Session: step.Session, Seq: step.Seq, Kept: step.Keep.Name}, true
	case step.Ask != nil, step.Open != nil:
		s.setups.wait(run, step.Seq)
	}
	return line, true
}

// setupReply passes the person's answer on to the step waiting for it.
func (s *apiServer) setupReply(ctx context.Context, req api.Request) api.Response {
	run, err := s.setups.take(req.Plugin, req.Session, req.Seq)
	if err != nil {
		return api.Response{Error: err.Error()}
	}
	select {
	case run.replied <- struct{}{}:
	default:
	}
	if err := run.inst.ReplySetup(ctx, run.session, req.Seq, req.Answer, ""); err != nil {
		s.setups.wait(run, req.Seq) // so that it can be sent again
		return api.Response{Error: fmt.Sprintf("could not reach %s: %v", req.Plugin, err)}
	}
	return api.Response{}
}
