package plugin

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"oikade.dev/contract/v1"
)

// Handler is a plugin's device logic, the same whether it runs as an outside process or as a
// plugin your home imports.
type Handler interface {
	// Serve reports devices, availability and state, then follows the devices until ctx ends.
	Serve(ctx context.Context, p *Plugin) error
	// Set applies a command and reports the result with SetState or UpdateState.
	Set(ctx context.Context, p *Plugin, cmd contract.SetCommand) error
}

// Factory builds a plugin's Handler. devices are the ones Oikade has on record for the plugin: from
// the devices file for an outside process, and from the device registry for a plugin your home
// imports, where they're empty the first time. A plugin that can list its devices reports them all,
// on record or not, and is configured in Go by the home that imports it. An error stops the plugin
// before it comes online.
type Factory func(env Environment, devices []contract.DeclaredDevice) (Handler, error)

// Run is an outside plugin's main: it reads the environment and any devices file, builds the
// Handler, starts, serves, and on SIGTERM or SIGINT closes and exits 0. Any error exits 1.
func Run(newHandler Factory) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	code := run(ctx, slog.New(slog.NewJSONHandler(os.Stderr, nil)), Options{}, newHandler)
	stop()
	os.Exit(code)
}

// run is Run until ctx ends, returning the exit code. o is Start's options before the environment,
// handler and logger are filled in.
func run(ctx context.Context, log *slog.Logger, o Options, newHandler Factory) int {
	failed := func(err error) int {
		log.Error("Could not start plugin", "err", err)
		return 1
	}
	env, err := ReadEnvironment()
	if err != nil {
		return failed(err)
	}
	var devices []contract.DeclaredDevice
	if env.DevicesFile != "" {
		if devices, err = ReadDevices(env.DevicesFile); err != nil {
			return failed(err)
		}
	}
	h, err := newHandler(env, devices)
	if err != nil {
		return failed(err)
	}
	o.Environment, o.OnSet, o.Logger = env, h.Set, log
	o.OnSetup = onSetup(h)
	p, err := Start(ctx, o)
	if err != nil {
		if ctx.Err() != nil {
			return 0 // stopped while starting; Start left any online status to the Last Will
		}
		return failed(err)
	}

	return serve(ctx, log, h, p)
}

// onSetup is h's Setup, or nil if it has none.
func onSetup(h Handler) func(context.Context, *Plugin, *Session) (string, error) {
	if s, ok := h.(Setup); ok {
		return s.Setup
	}
	return nil
}

// serve runs h until ctx ends, closes p, and returns the exit code.
func serve(ctx context.Context, log *slog.Logger, h Handler, p *Plugin) int {
	code := 0
	if err := h.Serve(ctx, p); err != nil && (!errors.Is(err, context.Canceled) || ctx.Err() == nil) {
		log.Error("Could not serve plugin", "err", err)
		code = 1
	} else {
		<-ctx.Done() // Serve may return before ctx ends once it has nothing to follow
	}
	if err := p.Close(context.Background()); err != nil {
		log.Error("Could not close plugin", "err", err)
		code = 1
	}
	return code
}
