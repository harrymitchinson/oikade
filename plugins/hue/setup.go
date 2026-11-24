package hue

import (
	"cmp"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"oikade.dev/internal/mdns"
	"oikade.dev/sdk/plugin"
)

var _ plugin.Setup = (*bridge)(nil)

// found is a bridge on the network: its ID, its model, such as BSB003, and where it is.
type found struct{ id, model, address string }

func (f found) String() string {
	switch {
	case f.id == "":
		return "the Hue bridge at " + f.address
	case f.model == "BSB002":
		return "Hue Bridge " + f.id
	case f.model == "BSB003":
		return "Hue Bridge Pro " + f.id
	}
	return "the Hue bridge " + f.id
}

// devicesWith reads the bridge with key, and is how many devices the plugin reports from it.
func (b *bridge) devicesWith(ctx context.Context, bridge found, key string, roots *x509.CertPool) (int, error) {
	c := newClient(bridge.address, key, bridgeTLS(roots, bridge.id), b.timing)
	defer c.close()
	raw, err := c.resources(ctx)
	if err != nil {
		return 0, err
	}
	h, _, err := build(raw)
	if err != nil {
		return 0, err
	}
	return len(h.devices), nil
}

// unreachable is an error reaching the bridge, as setup says it.
func (b *bridge) unreachable(bridge found, err error) error {
	if e := certFailure(err); e != nil {
		return fmt.Errorf("%s has a certificate that %w", bridge, e)
	}
	return fmt.Errorf("could not reach %s: %w", bridge, err)
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// lookForBridges is every bridge that answers over mDNS within that time, or as soon as want does.
func lookForBridges(ctx context.Context, within time.Duration, want string) ([]found, error) {
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	enough := func(s []mdns.Service) bool {
		return want != "" && slices.ContainsFunc(s, func(s mdns.Service) bool { return strings.EqualFold(s.TXT["bridgeid"], want) })
	}
	services, err := mdns.Browse(ctx, "_hue._tcp.local", enough)
	if err != nil {
		return nil, err
	}
	var all []found
	for _, s := range services {
		id := strings.ToUpper(s.TXT["bridgeid"])
		if !bridgeID(id) || slices.ContainsFunc(all, func(f found) bool { return f.id == id }) {
			continue
		}
		all = append(all, found{id: id, model: s.TXT["modelid"], address: serviceAddress(s)})
	}
	slices.SortFunc(all, func(a, b found) int { return cmp.Compare(a.id, b.id) })
	return all, nil
}

// Setup implements plugin.Setup. It finds the bridge, asks for a key once its link button is pressed,
// checks the key with a read, and keeps it with the bridge's ID. A key kept before that still works
// is left as it is.
func (b *bridge) Setup(ctx context.Context, _ *plugin.Plugin, s *plugin.Session) (string, error) {
	had, hadErr := b.kept()
	if hadErr != nil {
		had = kept{}
	}
	bridge, err := b.choose(ctx, s, had.Bridge)
	if err != nil {
		return "", err
	}
	roots := cmp.Or(b.config.roots, hueRoots)
	if had.Bridge == bridge.id {
		n, err := b.devicesWith(ctx, bridge, had.Key, roots)
		switch {
		case err == nil:
			return fmt.Sprintf("%s was set up already, and its key still works: %s", b.name(), plural(n, "device")), nil
		case !refusedKey(err):
			return "", b.unreachable(bridge, err)
		}
		if err := s.Say(ctx, "The bridge refused the key setup kept before, which may have been removed in the Hue app, so setup asks for another"); err != nil {
			return "", err
		}
	}
	if err := s.Say(ctx, "Press the link button on the bridge"); err != nil {
		return "", err
	}
	key, err := b.link(ctx, bridge, roots)
	if err != nil {
		return "", err
	}
	n, err := b.devicesWith(ctx, bridge, key, roots)
	if err != nil {
		return "", fmt.Errorf("could not read %s with the key it gave: %w", bridge, err)
	}
	raw, err := json.Marshal(kept{Bridge: bridge.id, Key: key})
	if err != nil {
		return "", err
	}
	if err := s.KeepSecret(ctx, b.secret(), string(raw)); err != nil {
		return "", fmt.Errorf("could not keep the bridge's key: %w", err)
	}
	select {
	case b.wake <- struct{}{}:
	default:
	}
	return fmt.Sprintf("%s is set up: %s", b.name(), plural(n, "device")), nil
}

// choose is the bridge at Address or, without one, the bridge setup kept before if it answers over
// mDNS, the only one that answers, or the one the person names.
func (b *bridge) choose(ctx context.Context, s *plugin.Session, had string) (found, error) {
	var bridge found
	if b.config.Address != "" {
		if err := s.Say(ctx, "Reaching the Hue bridge at "+b.config.Address); err != nil {
			return found{}, err
		}
		reach, cancel := context.WithTimeout(ctx, b.timing.request)
		defer cancel()
		var err error
		if bridge, err = identify(reach, b.config.Address, cmp.Or(b.config.roots, hueRoots)); err != nil {
			return found{}, b.unreachable(found{address: b.config.Address}, err)
		}
	} else {
		if err := s.Say(ctx, "Looking for Hue bridges over mDNS"); err != nil {
			return found{}, err
		}
		all, err := b.bridges(ctx, b.timing.browse, had)
		if err != nil {
			return found{}, fmt.Errorf("could not look for Hue bridges over mDNS: %w", err)
		}
		i := slices.IndexFunc(all, func(f found) bool { return f.id == had })
		switch {
		case i >= 0:
			bridge = all[i]
		case had != "" && len(all) > 0:
			// Its answer may be late: only the person knows whether to set up another.
			if err := s.Say(ctx, "The Hue bridge "+had+", which setup kept before, didn't answer"); err != nil {
				return found{}, err
			}
			return b.ask(ctx, s, all)
		case len(all) == 0:
			return found{}, errors.New("found no Hue bridge on the network over mDNS: check it's on and on the host's network, " +
				"or set hue.Config's Address")
		case len(all) == 1:
			bridge = all[0]
		default:
			return b.ask(ctx, s, all)
		}
	}
	return bridge, s.Say(ctx, "Found "+bridge.String())
}

// ask asks the person which of the bridges to set up.
func (b *bridge) ask(ctx context.Context, s *plugin.Session, all []found) (found, error) {
	names := make([]string, 0, len(all))
	for _, f := range all {
		names = append(names, f.String())
	}
	line := fmt.Sprintf("Found %d Hue bridges: %s", len(all), strings.Join(names, ", "))
	if len(all) == 1 {
		line = "Found " + names[0]
	}
	if err := s.Say(ctx, line); err != nil {
		return found{}, err
	}
	answer, err := s.Ask(ctx, "Which bridge? Type its ID")
	if err != nil {
		return found{}, err
	}
	answer = strings.TrimSpace(answer)
	if i := slices.IndexFunc(all, func(f found) bool { return strings.EqualFold(f.id, answer) }); i >= 0 {
		return all[i], nil
	}
	return found{}, fmt.Errorf("%q isn't one of the bridges' IDs: run setup again, and type one of them, or set hue.Config's Address", answer)
}

// link asks the bridge for a key until its link button is pressed, for up to timing's press.
func (b *bridge) link(ctx context.Context, bridge found, roots *x509.CertPool) (string, error) {
	c := newClient(bridge.address, "", bridgeTLS(roots, bridge.id), b.timing)
	defer c.close()
	// The Hue app lists the key under this name: an app's is up to 20 characters, and its instance's
	// up to 19.
	devicetype := "oikade#" + b.name()[:min(len(b.name()), 19)]
	until := time.Now().Add(b.timing.press)
	for {
		key, err := c.link(ctx, devicetype)
		switch {
		case err == nil:
			return key, nil
		case errors.Is(err, errNoLink):
			return "", fmt.Errorf("%s %w", bridge, err)
		case !errors.Is(err, errNotPressed):
			return "", b.unreachable(bridge, err)
		case time.Now().After(until):
			return "", fmt.Errorf("the link button on %s wasn't pressed within %s: run setup again, and press it then", bridge, b.timing.press)
		}
		sleep(ctx, b.timing.link)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
}
