package overkiz

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"

	"oikade.dev/contract/v1"
	"oikade.dev/sdk/plugin"
)

// clouds is a Cozytouch account on both of Atlantic's clouds, Overkiz and Magellan, each followed
// on its own after one login between them. An account has its devices on one or both, and a cloud
// that has no account for the login reports nothing and isn't a problem.
type clouds struct {
	overkiz  *bridge
	magellan *magellan
}

var _ plugin.Handler = (*clouds)(nil)

func newClouds(c Cozytouch, declared []contract.DeclaredDevice, t timing, log *slog.Logger) *clouds {
	c.signer = &signer{hold: t.refused}
	return &clouds{overkiz: newBridge(c, declared, t, log), magellan: newMagellan(c, t, log)}
}

// Serve implements plugin.Handler. It follows both clouds until ctx ends, or until the devices on
// record can't be reported as it starts.
func (c *clouds) Serve(ctx context.Context, p *plugin.Plugin) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s := &share{p: p}
	overkiz, magellan := s.part(), s.part()
	errs := make(chan error, 2)
	go func() { errs <- c.overkiz.serve(ctx, overkiz) }()
	go func() {
		c.magellan.serve(ctx, magellan)
		errs <- nil
	}()
	first := <-errs
	cancel()
	second := <-errs
	if first != nil {
		return first
	}
	return second
}

// Set implements plugin.Handler, on whichever cloud has the device.
func (c *clouds) Set(ctx context.Context, p *plugin.Plugin, cmd contract.SetCommand) error {
	if strings.HasPrefix(cmd.ID, magellanIDPrefix) {
		return c.magellan.set(ctx, cmd)
	}
	return c.overkiz.Set(ctx, p, cmd)
}

// share is one plugin reported to by several clouds, each through its part: the plugin's devices,
// discovered list and problem are the parts' together.
type share struct {
	p *plugin.Plugin

	// mu is held through a report to the plugin, so two parts' reports reach it in order.
	mu    sync.Mutex
	parts []*part
}

func (s *share) part() *part {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &part{share: s}
	s.parts = append(s.parts, p)
	return p
}

// part is one cloud's share of the plugin.
type part struct {
	share *share
	// discovered is nil until the cloud has seen its app, and while it can't.
	discovered []contract.DiscoveredDevice
	devices    []contract.DeviceReport
	problem    string
}

var _ reporter = (*part)(nil)

// SetDiscovered reports every part's list, or nil while any part can't see its app: a device
// missing from the list would say it was removed from it.
func (p *part) SetDiscovered(ctx context.Context, discovered []contract.DiscoveredDevice) error {
	s := p.share
	s.mu.Lock()
	defer s.mu.Unlock()
	p.discovered = slices.Clone(discovered)
	if discovered != nil && p.discovered == nil {
		p.discovered = []contract.DiscoveredDevice{}
	}
	var all []contract.DiscoveredDevice
	seen := map[string]bool{}
	for _, q := range s.parts {
		if q.discovered == nil {
			return s.p.SetDiscovered(ctx, nil)
		}
		for _, d := range q.discovered {
			if !seen[d.ID] {
				seen[d.ID] = true
				all = append(all, d)
			}
		}
	}
	if all == nil {
		all = []contract.DiscoveredDevice{}
	}
	return s.p.SetDiscovered(ctx, all)
}

// SetDevices reports every part's devices. A device on record that one part doesn't find and
// another does is found.
func (p *part) SetDevices(ctx context.Context, devices []contract.DeviceReport, _ int) error {
	s := p.share
	s.mu.Lock()
	defer s.mu.Unlock()
	p.devices = slices.Clone(devices)
	byID := map[string]contract.DeviceReport{}
	for _, q := range s.parts {
		for _, d := range q.devices {
			if had, ok := byID[d.ID]; !ok || (!had.Found && d.Found) {
				byID[d.ID] = d
			}
		}
	}
	all := make([]contract.DeviceReport, 0, len(byID))
	for _, id := range slices.Sorted(maps.Keys(byID)) {
		all = append(all, byID[id])
	}
	return s.p.SetDevices(ctx, all, 0)
}

func (p *part) SetAvailability(ctx context.Context, id string, online bool) error {
	return p.share.p.SetAvailability(ctx, id, online)
}

func (p *part) ReportState(ctx context.Context, id string, st contract.State) error {
	return p.share.p.ReportState(ctx, id, st)
}

// SetError reports every part's problem, each once: a password both refused is one problem.
func (p *part) SetError(ctx context.Context, problem string) error {
	s := p.share
	s.mu.Lock()
	defer s.mu.Unlock()
	p.problem = problem
	var problems []string
	for _, q := range s.parts {
		if q.problem != "" && !slices.Contains(problems, q.problem) {
			problems = append(problems, q.problem)
		}
	}
	return s.p.SetError(ctx, strings.Join(problems, "; "))
}
