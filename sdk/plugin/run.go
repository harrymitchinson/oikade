/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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

// Run is a plugin's main: it reads the environment and devices file, builds the handler, starts,
// serves, and on SIGTERM or SIGINT closes and exits 0. Any error exits 1.
func Run(newHandler func(env Environment, devices []contractv1.DeclaredDevice) (Handler, error)) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	code := run(ctx, slog.New(slog.NewJSONHandler(os.Stderr, nil)), Options{}, newHandler)
	stop()
	os.Exit(code)
}

// run is Run until ctx ends, returning the exit code. o is Start's options before the environment,
// handler and logger are filled in.
func run(ctx context.Context, log *slog.Logger, o Options,
	newHandler func(env Environment, devices []contractv1.DeclaredDevice) (Handler, error)) int {
	failed := func(err error) int {
		log.Error("Could not start plugin", "error", err.Error())
		return 1
	}
	env, err := ReadEnvironment()
	if err != nil {
		return failed(err)
	}
	devices, err := ReadDevices(env.DevicesFile)
	if err != nil {
		return failed(err)
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

	code := 0
	if err := h.Serve(ctx, p); err != nil && (!errors.Is(err, context.Canceled) || ctx.Err() == nil) {
		log.Error("Could not serve plugin", "error", err.Error())
		code = 1
	} else {
		<-ctx.Done() // Serve may return before ctx ends once it has nothing to follow
	}
	if err := p.Close(context.Background()); err != nil {
		log.Error("Could not close plugin", "error", err.Error())
		code = 1
	}
	return code
}
