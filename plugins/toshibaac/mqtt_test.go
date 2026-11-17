package toshibaac

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// pipeServer is the far end of a connection, which the test speaks MQTT on by hand.
type pipeServer struct {
	t *testing.T
	c *mqttConn
}

func pipe(t *testing.T) (net.Conn, *pipeServer) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return client, &pipeServer{t: t, c: &mqttConn{conn: server, r: bufio.NewReader(server)}}
}

func (s *pipeServer) expect(kind byte) []byte {
	s.t.Helper()
	header, body, err := s.c.read()
	if err != nil {
		s.t.Errorf("reading a packet of type %d: %v", kind, err)
		return nil
	}
	if header>>4 != kind {
		s.t.Errorf("a packet of type %d, not %d", header>>4, kind)
	}
	return body
}

func (s *pipeServer) send(header byte, body []byte) {
	s.t.Helper()
	if err := s.c.write(header, body); err != nil {
		s.t.Error(err)
	}
}

func TestConnectSendsTheCredentialsAndReadsTheCONNACK(t *testing.T) {
	for _, tt := range []struct {
		code    byte
		refused bool
		ok      bool
	}{{0, false, true}, {4, true, false}, {5, true, false}, {3, false, false}} {
		client, server := pipe(t)
		go func() {
			body := server.expect(pktConnect)
			want := append(appendString(nil, "MQTT"), 4, 0xC2, 0, 60)
			want = appendString(appendString(appendString(want, "device"), "hub/device/?api-version=x"), "token")
			if !bytes.Equal(body, want) {
				t.Errorf("CONNECT %x, want %x", body, want)
			}
			server.send(pktConnack<<4, []byte{0, tt.code})
		}()
		c, err := mqttConnect(t.Context(), client, "device", "hub/device/?api-version=x", "token", time.Minute)
		switch {
		case tt.ok && err != nil:
			t.Errorf("CONNACK %d: %v", tt.code, err)
		case !tt.ok && err == nil:
			t.Errorf("CONNACK %d: no error", tt.code)
		case errors.Is(err, errConnRefused) != tt.refused:
			t.Errorf("CONNACK %d: %v", tt.code, err)
		}
		if c != nil {
			_ = c.close()
		}
	}
}

func TestASubscriptionTheHubRefusesFails(t *testing.T) {
	client, server := pipe(t)
	c := &mqttConn{conn: client, r: bufio.NewReader(client), keepAlive: time.Minute}
	go func() {
		body := server.expect(pktSubscribe)
		if want := append(append([]byte{0, 1}, appendString(nil, methodsFilter)...), 0); !bytes.Equal(body, want) {
			t.Errorf("SUBSCRIBE %x, want %x", body, want)
		}
		// Anything before the SUBACK is skipped.
		server.send(pktPingresp<<4, nil)
		server.send(pktSuback<<4, []byte{0, 1, 0x80})
	}()
	if err := c.subscribe(t.Context(), methodsFilter); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("subscribe: %v", err)
	}
}

func TestRunAcknowledgesQoS1AndPings(t *testing.T) {
	client, server := pipe(t)
	c := &mqttConn{conn: client, r: bufio.NewReader(client), keepAlive: time.Second}
	got := make(chan string, 2)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- c.run(ctx, func(topic string, payload []byte) { got <- topic + " " + string(payload) })
	}()

	// QoS 1, packet ID 9, with a payload over 127 bytes, so its length takes two.
	long := strings.Repeat("x", 200)
	server.send(pktPublish<<4|0x02, append(append(appendString(nil, "a/b"), 0, 9), long...))
	if body := server.expect(pktPuback); !bytes.Equal(body, []byte{0, 9}) {
		t.Errorf("PUBACK %x", body)
	}
	if m := <-got; m != "a/b "+long {
		t.Errorf("got %q", m)
	}
	server.expect(pktPingreq)
	server.send(pktPingresp<<4, nil)
	cancel()
	// Ending sends a DISCONNECT.
	server.expect(pktDisconnect)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("run: %v", err)
	}
}

func TestRunFailsWhenTheHubGoesQuiet(t *testing.T) {
	client, server := pipe(t)
	c := &mqttConn{conn: client, r: bufio.NewReader(client), keepAlive: time.Second}
	done := make(chan error, 1)
	go func() { done <- c.run(t.Context(), func(string, []byte) {}) }()
	// The ping is read, and never answered.
	server.expect(pktPingreq)
	select {
	case err := <-done:
		if err == nil {
			t.Error("no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run went on without the hub")
	}
}

func TestAPacketThatIsntMQTTFails(t *testing.T) {
	for name, raw := range map[string][]byte{
		"a length over four bytes": {pktPublish << 4, 0xFF, 0xFF, 0xFF, 0xFF, 0x01},
		"a length over the limit":  {pktPublish << 4, 0xFF, 0xFF, 0xFF, 0x7F},
	} {
		client, server := pipe(t)
		c := &mqttConn{conn: client, r: bufio.NewReader(client)}
		go func() { _, _ = server.c.conn.Write(raw) }()
		if _, _, err := c.read(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	for name, body := range map[string][]byte{
		"no topic length":   {0},
		"a short topic":     {0, 5, 'a'},
		"no packet ID":      {0, 1, 'a', 0},
		"just a topic, QoS": {0, 1, 'a'},
	} {
		header := byte(pktPublish << 4)
		if strings.Contains(name, "ID") || strings.Contains(name, "QoS") {
			header |= 0x02
		}
		if _, _, _, err := parsePublish(header, body); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestAPublishAtQoS1WaitsForItsPUBACK(t *testing.T) {
	client, server := pipe(t)
	c := &mqttConn{conn: client, r: bufio.NewReader(client), keepAlive: time.Minute, waiting: map[uint16]chan error{}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.run(ctx, func(string, []byte) {}) }()

	published := make(chan error, 1)
	go func() { published <- c.publishAcked(t.Context(), "a/b", []byte("hi")) }()
	header, body, err := server.c.read()
	if err != nil {
		t.Fatal(err)
	}
	// QoS 1, and the first ID after the subscription's.
	if want := append(appendString(nil, "a/b"), 0, 2, 'h', 'i'); header != pktPublish<<4|0x02 || !bytes.Equal(body, want) {
		t.Errorf("PUBLISH %x %x, want %x", header, body, want)
	}
	// Another ID's PUBACK isn't its.
	server.send(pktPuback<<4, []byte{0, 7})
	select {
	case err := <-published:
		t.Fatalf("returned %v on another's PUBACK", err)
	case <-time.After(50 * time.Millisecond):
	}
	server.send(pktPuback<<4, []byte{0, 2})
	if err := <-published; err != nil {
		t.Errorf("publish: %v", err)
	}
	// A PUBACK sent twice blocks nothing: the next publish is still heard.
	server.send(pktPuback<<4, []byte{0, 2})
	go func() { published <- c.publishAcked(t.Context(), "a/b", nil) }()
	if _, _, err := server.c.read(); err != nil {
		t.Fatal(err)
	}
	server.send(pktPuback<<4, []byte{0, 3})
	if err := <-published; err != nil {
		t.Errorf("publish after a repeated PUBACK: %v", err)
	}

	// One that's never acknowledged gives up when its context does.
	short, stop := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer stop()
	go func() { published <- c.publishAcked(short, "a/b", nil) }()
	if _, _, err := server.c.read(); err != nil {
		t.Fatal(err)
	}
	if err := <-published; err == nil || !strings.Contains(err.Error(), "no PUBACK") {
		t.Errorf("publish without a PUBACK: %v", err)
	}

	// And one waiting when the connection ends fails with it.
	go func() { published <- c.publishAcked(t.Context(), "a/b", nil) }()
	if _, _, err := server.c.read(); err != nil {
		t.Fatal(err)
	}
	_ = server.c.conn.Close()
	if err := <-published; err == nil || !strings.Contains(err.Error(), "ended") {
		t.Errorf("publish as the connection ended: %v", err)
	}
	if err := <-done; err == nil {
		t.Error("run ended without an error")
	}
	if err := c.publishAcked(t.Context(), "a/b", nil); err == nil {
		t.Error("a publish after the end: no error")
	}
}
