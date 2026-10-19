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

// Package mqtt is a small MQTT client for the Oikade data plane.
package mqtt

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
	"github.com/go-logr/logr"
)

// Message is a received MQTT message.
type Message struct {
	Topic    string
	Payload  []byte
	Retained bool
}

// Handler receives messages. Handlers run on the client's receive goroutine in arrival order,
// so they must not block.
type Handler func(Message)

// Bus is the subset of MQTT the rest of Oikade uses.
type Bus interface {
	Publish(ctx context.Context, topic string, payload []byte, retain bool) error
	// Subscribe registers h for filter (MQTT wildcards allowed). Subscriptions are restored after
	// a reconnect, and retained messages are delivered on subscribe. Calling the returned func
	// removes the handler.
	Subscribe(ctx context.Context, filter string, h Handler) (unsubscribe func(), err error)
}

type subscription struct {
	id     uint64
	filter string
	h      Handler
}

// Client is a Bus backed by a reconnecting MQTT v5 connection.
type Client struct {
	cm        *autopaho.ConnectionManager
	onConnect func(ctx context.Context)
	// ready is closed once cm is set, so OnConnect can publish.
	ready chan struct{}

	mu     sync.RWMutex
	subs   []subscription
	nextID uint64
	// cancelConn ends the current connection's context.
	cancelConn context.CancelFunc
	// ups counts connections; subscriptions only need restoring after the first.
	ups atomic.Int64
	// upMu serialises the work done on each connection, so a stale one can't overlap the next.
	upMu sync.Mutex
}

var _ Bus = (*Client)(nil)

// Option configures Connect.
type Option func(*options)

type options struct {
	will      *paho.WillMessage
	onConnect func(ctx context.Context)
}

// WithWill sets a Last Will the broker publishes, retained at QoS 1, if the connection drops
// without Disconnect.
func WithWill(topic string, payload []byte) Option {
	return func(o *options) { o.will = &paho.WillMessage{Topic: topic, Payload: payload, QoS: 1, Retain: true} }
}

// WithOnConnect runs f on the first and every later connection, after subscriptions are restored.
// Runs never overlap. f's context is cancelled when that connection drops.
func WithOnConnect(f func(ctx context.Context)) Option {
	return func(o *options) { o.onConnect = f }
}

// Connect dials server (mqtt://host:port) and blocks until the first connection succeeds or ctx ends.
// The connection is retried forever in the background after that, until ctx is cancelled or
// Disconnect is called. It doesn't wait for OnConnect. The logger comes from ctx.
func Connect(ctx context.Context, server, clientID string, opts ...Option) (*Client, error) {
	log := logr.FromContextOrDiscard(ctx)
	u, err := url.Parse(server)
	if err != nil {
		return nil, fmt.Errorf("parsing MQTT URL: %w", err)
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	c := &Client{onConnect: o.onConnect, ready: make(chan struct{})}
	cfg := autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{u},
		KeepAlive:                     30,
		CleanStartOnInitialConnection: true,
		ReconnectBackoff:              autopaho.NewConstantBackoff(5 * time.Second),
		WillMessage:                   o.will,
		OnConnectionUp: func(cm *autopaho.ConnectionManager, _ *paho.Connack) {
			log.Info("Connected to MQTT broker", "server", u.Host)
			connCtx, cancel := context.WithCancel(ctx)
			c.mu.Lock()
			c.cancelConn = cancel
			c.mu.Unlock()
			// autopaho waits for this callback, so the work runs on its own goroutine.
			go c.connectionUp(connCtx, cm, c.ups.Add(1) == 1)
		},
		OnConnectionDown: func() bool {
			c.endConnection()
			return true
		},
		OnConnectError: func(err error) { log.Error(err, "Failed to connect to MQTT broker", "server", u.Host) },
		ClientConfig: paho.ClientConfig{
			ClientID: clientID,
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){
				func(pr paho.PublishReceived) (bool, error) {
					c.dispatch(Message{Topic: pr.Packet.Topic, Payload: pr.Packet.Payload, Retained: pr.Packet.Retain})
					return true, nil
				},
			},
		},
	}
	if u.User != nil {
		pw, _ := u.User.Password()
		cfg.ConnectUsername, cfg.ConnectPassword = u.User.Username(), []byte(pw)
	}
	cm, err := autopaho.NewConnection(ctx, cfg)
	if err != nil {
		return nil, err
	}
	c.cm = cm
	close(c.ready)
	if err := cm.AwaitConnection(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// Publish sends a QoS 1 message.
func (c *Client) Publish(ctx context.Context, topic string, payload []byte, retain bool) error {
	_, err := c.cm.Publish(ctx, &paho.Publish{Topic: topic, Payload: payload, QoS: 1, Retain: retain})
	return err
}

// Subscribe implements Bus. The handler stays registered even if subscribing fails, so it is
// restored on the next reconnect.
func (c *Client) Subscribe(ctx context.Context, filter string, h Handler) (func(), error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.subs = append(c.subs, subscription{id: id, filter: filter, h: h})
	c.mu.Unlock()
	unsubscribe := func() { c.unsubscribe(id) }
	_, err := c.cm.Subscribe(ctx, &paho.Subscribe{Subscriptions: []paho.SubscribeOptions{{Topic: filter, QoS: 1}}})
	return unsubscribe, err
}

func (c *Client) unsubscribe(id uint64) {
	c.mu.Lock()
	i := slices.IndexFunc(c.subs, func(s subscription) bool { return s.id == id })
	if i < 0 {
		c.mu.Unlock()
		return
	}
	filter := c.subs[i].filter
	c.subs = slices.Delete(c.subs, i, i+1)
	shared := slices.ContainsFunc(c.subs, func(s subscription) bool { return s.filter == filter })
	c.mu.Unlock()
	if !shared {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = c.cm.Unsubscribe(ctx, &paho.Unsubscribe{Topics: []string{filter}})
	}
}

// Disconnect sends a clean DISCONNECT, so the broker doesn't publish the Will, and stops
// reconnecting.
func (c *Client) Disconnect(ctx context.Context) error {
	c.endConnection()
	return c.cm.Disconnect(ctx)
}

func (c *Client) endConnection() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancelConn != nil {
		c.cancelConn()
	}
}

// Done is closed once the connection has shut down after ctx is cancelled or Disconnect.
func (c *Client) Done() <-chan struct{} { return c.cm.Done() }

// connectionUp restores subscriptions after a reconnect, then runs OnConnect.
func (c *Client) connectionUp(ctx context.Context, cm *autopaho.ConnectionManager, first bool) {
	c.upMu.Lock()
	defer c.upMu.Unlock()
	if !first {
		c.resubscribe(ctx, cm)
	}
	if c.onConnect == nil {
		return
	}
	select {
	case <-c.ready:
	case <-ctx.Done():
		return
	}
	if ctx.Err() == nil {
		c.onConnect(ctx)
	}
}

func (c *Client) resubscribe(ctx context.Context, cm *autopaho.ConnectionManager) {
	c.mu.RLock()
	opts := make([]paho.SubscribeOptions, 0, len(c.subs))
	for _, s := range c.subs {
		opts = append(opts, paho.SubscribeOptions{Topic: s.filter, QoS: 1})
	}
	c.mu.RUnlock()
	if len(opts) == 0 {
		return
	}
	if _, err := cm.Subscribe(ctx, &paho.Subscribe{Subscriptions: opts}); err != nil {
		logr.FromContextOrDiscard(ctx).Error(err, "Failed to restore MQTT subscriptions")
	}
}

func (c *Client) dispatch(m Message) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, s := range c.subs {
		if Match(s.filter, m.Topic) {
			s.h(m)
		}
	}
}

// Match reports whether topic matches an MQTT filter with + and # wildcards.
func Match(filter, topic string) bool {
	f, t := strings.Split(filter, "/"), strings.Split(topic, "/")
	for i, part := range f {
		switch {
		case part == "#":
			return true
		case i >= len(t):
			return false
		case part != "+" && part != t[i]:
			return false
		}
	}
	return len(f) == len(t)
}
