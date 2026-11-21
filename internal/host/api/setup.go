package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"oikade.dev/contract/v1"
)

// SetupStep is one line of a setup's stream: the plugin's step, as the host passes it on. The first
// line has only Session, and says the setup has started. It's the stream of POST /v1/setup, which
// other clients may read: add to it, and don't rename.
type SetupStep struct {
	Session string `json:"session"`
	Seq     int    `json:"seq,omitempty"`
	Say     string `json:"say,omitempty"`
	// Ask and Open need a SetupReply naming Seq.
	Ask  *contract.SetupAsk  `json:"ask,omitempty"`
	Open *contract.SetupOpen `json:"open,omitempty"`
	// Kept is a secret the host kept for the plugin, by name. Its value never leaves the host.
	Kept string `json:"kept,omitempty"`
	// Done ends the setup.
	Done *contract.SetupDone `json:"done,omitempty"`
}

// Setup runs plugin's setup on the host, and hands each step to each until it's done, which is the
// last step each gets. Answer an Ask or an Open with SetupReply, from each or from another
// goroutine. Ending ctx ends the setup. It returns ErrHostStopped if the host went first.
func Setup(ctx context.Context, t Transport, plugin string, each func(SetupStep) error) error {
	body, err := open(ctx, t, Request{Type: TypeSetup, Plugin: plugin})
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	steps := json.NewDecoder(body)
	for {
		var s SetupStep
		if err := steps.Decode(&s); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				return ErrHostStopped
			}
			return fmt.Errorf("lost the host: %w", err)
		}
		if err := each(s); err != nil {
			return err
		}
		if s.Done != nil {
			return nil
		}
	}
}

// SetupReply answers step seq of a plugin's setup session.
func SetupReply(ctx context.Context, t Transport, plugin, session string, seq int, answer string) error {
	resp, err := Call(ctx, t, Request{Type: TypeSetupReply, Plugin: plugin, Session: session, Seq: seq, Answer: answer})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	return nil
}
