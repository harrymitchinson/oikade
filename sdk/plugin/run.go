package plugin

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
)

// Handler is a plugin's device logic.
type Handler interface {
	// Serve reports devices, availability and state, then follows the devices until ctx ends.
	Serve(ctx context.Context, p *Plugin) error
	// Set applies a command and reports the result with SetState or UpdateState.
	Set(ctx context.Context, p *Plugin, cmd contractv1.SetCommand) error
}

// Factory builds a plugin's Handler. devices are the ones Oikade has on record for the plugin: from
// devices.json for an outside process, and from the device registry for a plugin your home imports,
// where they're empty the first time. A plugin that can list its devices reports them all, on record
// or not, and is configured in Go by the home that imports it.
type Factory func(env Environment, devices []contractv1.DeclaredDevice) (Handler, error)

// Run is a plugin's main: it reads the environment and devices file, builds the handler, starts,
// serves, and on SIGTERM or SIGINT closes and exits 0. Any error exits 1.
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
	var devices []contractv1.DeclaredDevice
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
	p, err := Start(ctx, o)
	if err != nil {
		if ctx.Err() != nil {
			return 0 // stopped while starting; Start left any online status to the Last Will
		}
		return failed(err)
	}

	return serve(ctx, log, h, p)
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
