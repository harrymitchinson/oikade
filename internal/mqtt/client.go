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
	// a reconnect, and retained messages are delivered on subscribe.
	Subscribe(ctx context.Context, filter string, h Handler) error
}

type subscription struct {
	filter string
	h      Handler
}

// Client is a Bus backed by a reconnecting MQTT v5 connection.
type Client struct {
	cm *autopaho.ConnectionManager

	mu   sync.RWMutex
	subs []subscription
	// ups counts connections; subscriptions only need restoring after the first.
	ups atomic.Int64
}

var _ Bus = (*Client)(nil)

// Connect dials server (mqtt://host:port) and blocks until the first connection succeeds or ctx ends.
// The connection is retried forever in the background after that, until ctx is cancelled.
// The logger comes from ctx.
func Connect(ctx context.Context, server, clientID string) (*Client, error) {
	log := logr.FromContextOrDiscard(ctx)
	u, err := url.Parse(server)
	if err != nil {
		return nil, fmt.Errorf("parsing MQTT URL: %w", err)
	}
	c := &Client{}
	cfg := autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{u},
		KeepAlive:                     30,
		CleanStartOnInitialConnection: true,
		ReconnectBackoff:              autopaho.NewConstantBackoff(5 * time.Second),
		OnConnectionUp: func(cm *autopaho.ConnectionManager, _ *paho.Connack) {
			log.Info("Connected to MQTT broker", "server", u.Host)
			if c.ups.Add(1) > 1 {
				go c.resubscribe(ctx, cm)
			}
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

// Subscribe implements Bus.
func (c *Client) Subscribe(ctx context.Context, filter string, h Handler) error {
	c.mu.Lock()
	c.subs = append(c.subs, subscription{filter: filter, h: h})
	c.mu.Unlock()
	_, err := c.cm.Subscribe(ctx, &paho.Subscribe{Subscriptions: []paho.SubscribeOptions{{Topic: filter, QoS: 1}}})
	return err
}

// Done is closed once the connection has shut down after ctx is cancelled.
func (c *Client) Done() <-chan struct{} { return c.cm.Done() }

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
