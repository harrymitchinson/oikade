package plugin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/host/pluginwire"
	"github.com/harrymitchinson/oikade/internal/mqtt"
)

const (
	// hostedDial bounds opening the session with the host.
	hostedDial = 10 * time.Second
	// hostedClose bounds saying goodbye. The host marks a plugin offline when it goes anyway.
	hostedClose = 2 * time.Second
)

// hostLink is a plugin's session with the host. It carries the same messages as a broker would, so
// a Handler is the same code either way, and the host checks them the same way.
type hostLink struct {
	conn *pluginwire.Conn[pluginwire.Message]
	// closing is this end going, which isn't the host being lost.
	closing atomic.Bool
}

func (l *hostLink) Publish(_ context.Context, topic string, payload []byte, retain bool) error {
	return l.conn.Write(pluginwire.Message{Type: pluginwire.TypePublish, Topic: topic, Data: payload, Retain: retain})
}

func (l *hostLink) Disconnect(context.Context) error {
	l.closing.Store(true)
	return l.conn.Close()
}

func (l *hostLink) Drop(ctx context.Context) error { return l.Disconnect(ctx) }

// RunHosted serves a plugin your home imports, as the process the host started for it, until ctx
// ends. framework.Run calls it: you don't need to. socket is the host's, and name the plugin's.
//
// A plugin that goes wrong takes only itself down: the host restarts this process. One that panics
// is gone at once. One that stops answering, or whose Set runs for too long, is killed.
func RunHosted(ctx context.Context, socket, name string, newHandler Factory, log *slog.Logger) error {
	dial, cancel := context.WithTimeout(ctx, hostedDial)
	defer cancel()
	conn, welcome, err := pluginwire.Dial(dial, socket, name)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	root, err := contractv1.ParseRoot(welcome.Topic)
	if err != nil {
		return err
	}

	life, stop := context.WithCancel(context.WithoutCancel(ctx))
	defer stop()
	serving, lost := context.WithCancelCause(ctx)
	defer lost(nil)
	// Answering starts first, so that a slow Factory isn't taken for a hung one.
	var current atomic.Pointer[Plugin]
	var reading sync.WaitGroup
	link := &hostLink{conn: conn}
	reading.Go(func() {
		if err := follow(conn, &current); !link.closing.Load() {
			lost(err)
		}
	})

	h, err := newHandler(Environment{
		MQTTURL: os.Getenv(contractv1.EnvMQTTURL), Root: root, SecretsDir: os.Getenv(contractv1.EnvSecretsDir),
	}, welcome.Declared)
	if err != nil {
		return err
	}
	p := newPlugin(root, log, h.Set, hostedClose, life, stop)
	p.client, p.setTimeout = link, welcome.SetTimeout
	close(p.ready)
	current.Store(p)
	p.announce(serving)
	select {
	case <-p.online:
	default:
		if err := context.Cause(serving); err != nil && ctx.Err() == nil {
			return fmt.Errorf("lost the host: %w", err)
		}
		if ctx.Err() != nil {
			return nil
		}
		return errors.New("could not announce the plugin")
	}
	code := serve(serving, log, h, p)
	_ = link.Disconnect(ctx)
	reading.Wait()
	switch {
	case ctx.Err() == nil && context.Cause(serving) != nil:
		return fmt.Errorf("lost the host: %w", context.Cause(serving))
	case code != 0:
		return errors.New("the plugin stopped serving; its log says why")
	}
	return nil
}

// follow reads from the host until the session ends, and returns why. A stuck Set stops the answers
// to pings, so the host replaces a process that looks alive and isn't.
func follow(conn *pluginwire.Conn[pluginwire.Message], current *atomic.Pointer[Plugin]) error {
	for {
		m, err := conn.Read()
		if err != nil {
			return err
		}
		p := current.Load()
		switch m.Type {
		case pluginwire.TypePing:
			if p != nil && p.stuck() {
				continue
			}
			if err := conn.Write(pluginwire.Message{Type: pluginwire.TypePong, ID: m.ID}); err != nil {
				return err
			}
		case pluginwire.TypePublish:
			if p != nil {
				p.receive(mqtt.Message{Topic: m.Topic, Payload: m.Data})
			}
		}
	}
}
