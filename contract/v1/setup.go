package v1

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// SetupIn is the topic Oikade sends a plugin's setup on: SetupRequest.
func (r Root) SetupIn() string { return string(r) + "/_setup/in" }

// SetupOut is the topic a plugin answers its setup on: SetupStep.
func (r Root) SetupOut() string { return string(r) + "/_setup/out" }

// SetupRequest is what Oikade sends on SetupIn. It starts a session, replies to one of its steps,
// or ends it.
type SetupRequest struct {
	// Session names the session. Every step and reply carries it.
	Session string `json:"session"`
	// Start begins the session. A plugin runs one at a time, so a new one ends the last.
	Start bool `json:"start,omitempty"`
	// Reply is the Seq of the step this answers: an ask, an open or a keep.
	Reply int `json:"reply,omitempty"`
	// Value is the answer: what the person typed, or an open's redirect query, such as
	// "code=abc&state=xyz".
	Value string `json:"value,omitempty"`
	// Error is why the step failed, such as a secret the host couldn't keep. Value is then empty.
	Error string `json:"error,omitempty"`
	// End stops the session, because the person went or took too long.
	End bool `json:"end,omitempty"`
}

// SetupStep is one thing a plugin's setup does, sent on SetupOut. It has exactly one of Say, Ask,
// Open, Keep and Done.
type SetupStep struct {
	// Session is the SetupRequest's session this step is for.
	Session string `json:"session"`
	// Seq numbers the session's steps from 1. A reply names the step it answers by it.
	Seq int `json:"seq"`
	// Say is a line to show the person.
	Say string `json:"say,omitempty"`
	// Ask is a question, answered by a reply.
	Ask *SetupAsk `json:"ask,omitempty"`
	// Open is a page to open in the person's browser, answered by a reply once it redirects.
	Open *SetupOpen `json:"open,omitempty"`
	// Keep is a secret for the host to keep, answered by a reply once it's on disk.
	Keep *SetupKeep `json:"keep,omitempty"`
	// Done ends the session.
	Done *SetupDone `json:"done,omitempty"`
}

// SetupAsk is a question for the person.
type SetupAsk struct {
	Prompt string `json:"prompt"`
	// Secret hides what the person types.
	Secret bool `json:"secret,omitempty"`
}

// SetupOpen is an https page to open, such as a sign-in. Oikade listens on Redirect, which is
// http://127.0.0.1:PORT/PATH or http://localhost:PORT/PATH, and replies with the query the page
// redirects there with.
type SetupOpen struct {
	URL      string `json:"url"`
	Redirect string `json:"redirect"`
}

// SetupKeep is a secret for the host to keep, such as a device's key, which the plugin reads from
// EnvSecretsDir from then on. Name is a ValidSecretName. Never log Value.
type SetupKeep struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// SetupDone is how a session ended: Result if it worked, Error if it didn't.
type SetupDone struct {
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// NeedsReply reports whether Oikade replies to the step.
func (s SetupStep) NeedsReply() bool { return s.Ask != nil || s.Open != nil || s.Keep != nil }

var secretName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)

// ValidSecretName reports whether name may name a secret: 1 to 253 letters, digits, '.', '_' or
// '-', starting with a letter or digit, so it can't leave the secrets directory or hide in it.
func ValidSecretName(name string) bool { return secretName.MatchString(name) }

// Validate checks a step as Oikade does before acting on it.
func (s SetupStep) Validate() error {
	if s.Session == "" {
		return errors.New("setup step has no session")
	}
	if s.Seq < 1 {
		return fmt.Errorf("setup step has seq %d, want 1 or more", s.Seq)
	}
	parts := 0
	for _, set := range []bool{s.Say != "", s.Ask != nil, s.Open != nil, s.Keep != nil, s.Done != nil} {
		if set {
			parts++
		}
	}
	if parts != 1 {
		return fmt.Errorf("setup step has %d of say, ask, open, keep and done, want 1", parts)
	}
	switch {
	case s.Ask != nil && s.Ask.Prompt == "":
		return errors.New("setup ask has no prompt")
	case s.Open != nil:
		return validOpen(*s.Open)
	case s.Keep != nil && !ValidSecretName(s.Keep.Name):
		return fmt.Errorf("setup keep's name %q isn't a secret's name", s.Keep.Name)
	case s.Done != nil && s.Done.Result != "" && s.Done.Error != "":
		return errors.New("setup done has both a result and an error")
	}
	return nil
}

func validOpen(o SetupOpen) error {
	u, err := url.Parse(o.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("setup open's url %q isn't an https URL", o.URL)
	}
	if _, err := RedirectAddress(o.Redirect); err != nil {
		return err
	}
	return nil
}

// RedirectAddress returns the address to listen on for an open's redirect, such as
// 127.0.0.1:8765. Only a plain http URL on this machine's loopback, with a port, will do.
func RedirectAddress(redirect string) (string, error) {
	u, err := url.Parse(redirect)
	if err != nil || u.Scheme != "http" || u.Port() == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("setup open's redirect %q isn't http://127.0.0.1:PORT/PATH", redirect)
	}
	if port, err := strconv.Atoi(u.Port()); err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("setup open's redirect %q has no valid port", redirect)
	}
	switch host := u.Hostname(); {
	case host == "127.0.0.1", strings.EqualFold(host, "localhost"):
		return net.JoinHostPort("127.0.0.1", u.Port()), nil
	default:
		return "", fmt.Errorf("setup open's redirect %q isn't on 127.0.0.1 or localhost", redirect)
	}
}
