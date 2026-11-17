package clusters

import (
	"fmt"
	"testing"

	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/matter/attestation"
	"github.com/harrymitchinson/oikade/internal/matter/contain"
	"github.com/harrymitchinson/oikade/internal/matter/fabric"
	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
)

// underHandler runs f as a protocol handler does, under contain.Recover, and returns what got past
// it: a panic that stops the host. contained is whether Recover contained one instead.
func underHandler(f func()) (escaped any, contained bool) {
	defer func() { escaped = recover() }()
	defer contain.Recover(logging.Discard(), contain.Handler, func(string) { contained = true }, nil)
	f()
	return nil, false
}

// stopsTheHost checks that f panics, and that the panic isn't contained.
func stopsTheHost(t *testing.T, name string, f func()) {
	t.Helper()
	escaped, contained := underHandler(f)
	switch {
	case contained:
		t.Errorf("%s: its panic was contained", name)
	case escaped == nil:
		t.Errorf("%s didn't panic", name)
	}
}

func TestCriticalStopsTheHost(t *testing.T) {
	ok := false
	cl := &im.Cluster{
		Commands: []im.Command{
			{ID: 1, Invoke: func(*im.Request, *tlv.Reader) (*im.Response, error) { panic("injected") }},
			{ID: 2, Invoke: func(*im.Request, *tlv.Reader) (*im.Response, error) { ok = true; return nil, nil }},
		},
		Attributes: []im.Attribute{
			{ID: 1, Write: func(*im.Request, *tlv.Reader, im.WriteOp) im.Status { panic("injected") }},
		},
	}
	critical(cl)
	stopsTheHost(t, "a command", func() { _, _ = cl.Commands[0].Invoke(nil, nil) })
	stopsTheHost(t, "a write", func() { _ = cl.Attributes[0].Write(nil, nil, im.Replace) })
	if escaped, contained := underHandler(func() { _, _ = cl.Commands[1].Invoke(nil, nil) }); escaped != nil || contained || !ok {
		t.Errorf("a command that doesn't panic: %v, %v, ran %v", escaped, contained, ok)
	}
}

// TestCommissioningPanicsStopTheHost: a panic in a commissioning command or write isn't
// contained, though it runs on a handler, so it stops the host rather than leave a window, a
// fail-safe or the fabrics half moved. A write panics without a value to read, and
// OpenBasicCommissioningWindow without a request, once it has read its fields.
func TestCommissioningPanicsStopTheHost(t *testing.T) {
	table, err := fabric.Open(fabric.DirStore{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	creds, err := attestation.Test()
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewCommissioning(CommissioningConfig{Fabrics: table, Attestation: creds, Log: logging.Test(t)})
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	for _, cl := range c.Clusters() {
		for _, a := range cl.Attributes {
			if a.Write != nil {
				writes++
				stopsTheHost(t, fmt.Sprintf("%s attribute %#x", Name(cl.ID), a.ID), func() { _ = a.Write(nil, nil, im.Replace) })
			}
		}
	}
	if writes == 0 {
		t.Error("no commissioning attribute is writable")
	}

	var w tlv.Writer
	w.StartStructure(tlv.Anonymous)
	w.PutUint(tlv.Context(0), uint64(minWindowTimeout.Seconds()))
	w.End()
	fields := tlv.NewReader(w.Bytes())
	if err := fields.Next(); err != nil {
		t.Fatal(err)
	}
	stopsTheHost(t, "OpenBasicCommissioningWindow", func() {
		_, _ = command(t, c.admin, 0x01).Invoke(nil, fields)
	})
}

func command(t *testing.T, cl *im.Cluster, id uint32) im.Command {
	t.Helper()
	for _, cmd := range cl.Commands {
		if cmd.ID == id {
			return cmd
		}
	}
	t.Fatalf("%s has no command %#x", Name(cl.ID), id)
	return im.Command{}
}
