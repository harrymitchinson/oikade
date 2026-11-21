package toshibaac

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// mqttConn is the little of MQTT 3.1.1 an Azure IoT Hub device needs: connect, one subscription,
// publish at QoS 0 and 1, and keep-alive. The hub doesn't speak MQTT 5, which is all paho.golang does,
// and a 3.1.1 client this size is less to depend on than a library.
type mqttConn struct {
	conn net.Conn
	r    *bufio.Reader
	// keepAlive is how often to ping, and a read waits at most half as long again for anything.
	keepAlive time.Duration

	wmu sync.Mutex

	// mu guards the publishes at QoS 1 waiting for their PUBACK, by packet ID, and why the
	// connection ended, once it has.
	mu      sync.Mutex
	nextID  uint16
	waiting map[uint16]chan error
	ended   error
}

// The packet types, as the high nibble of a fixed header.
const (
	pktConnect    = 1
	pktConnack    = 2
	pktPublish    = 3
	pktPuback     = 4
	pktSubscribe  = 8
	pktSuback     = 9
	pktPingreq    = 12
	pktPingresp   = 13
	pktDisconnect = 14
)

// maxPacket bounds a packet the plugin reads. A unit's state is under a kilobyte.
const maxPacket = 256 << 10

// errConnRefused is a CONNACK that refused the credentials: return codes 4 and 5.
var errConnRefused = errors.New("the hub refused the plugin's credentials")

// mqttConnect sends CONNECT on conn, with a clean session, and waits for the CONNACK.
func mqttConnect(ctx context.Context, conn net.Conn, clientID, username, password string, keepAlive time.Duration) (*mqttConn, error) {
	c := &mqttConn{conn: conn, r: bufio.NewReader(conn), keepAlive: keepAlive, waiting: map[uint16]chan error{}}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
		defer func() { _ = conn.SetDeadline(time.Time{}) }()
	}
	var body []byte
	body = appendString(body, "MQTT")
	// Level 4 is 3.1.1. The flags are a username, a password and a clean session.
	body = append(body, 4, 0xC2)
	body = binary.BigEndian.AppendUint16(body, uint16(keepAlive/time.Second))
	body = appendString(body, clientID)
	body = appendString(body, username)
	body = appendString(body, password)
	if err := c.write(pktConnect<<4, body); err != nil {
		return nil, err
	}
	kind, payload, err := c.read()
	switch {
	case err != nil:
		return nil, fmt.Errorf("no CONNACK: %w", err)
	case kind>>4 != pktConnack || len(payload) != 2:
		return nil, fmt.Errorf("a packet of type %d in place of a CONNACK", kind>>4)
	case payload[1] == 4 || payload[1] == 5:
		return nil, fmt.Errorf("%w (CONNACK %d)", errConnRefused, payload[1])
	case payload[1] != 0:
		return nil, fmt.Errorf("the hub refused the connection (CONNACK %d)", payload[1])
	}
	return c, nil
}

// subscribe subscribes to filter at QoS 0, and waits for the SUBACK.
func (c *mqttConn) subscribe(ctx context.Context, filter string) error {
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetDeadline(deadline)
		defer func() { _ = c.conn.SetDeadline(time.Time{}) }()
	}
	body := binary.BigEndian.AppendUint16(nil, 1)
	body = appendString(body, filter)
	body = append(body, 0)
	// SUBSCRIBE's flags are always 0010.
	if err := c.write(pktSubscribe<<4|0x02, body); err != nil {
		return err
	}
	for {
		kind, payload, err := c.read()
		if err != nil {
			return fmt.Errorf("no SUBACK: %w", err)
		}
		// Nothing is routed to the plugin until it has subscribed, so anything else can be skipped.
		if kind>>4 != pktSuback {
			continue
		}
		if len(payload) != 3 || binary.BigEndian.Uint16(payload) != 1 || payload[2] == 0x80 {
			return fmt.Errorf("the hub refused the subscription to %s", filter)
		}
		return nil
	}
}

// publish sends a message at QoS 0.
func (c *mqttConn) publish(topic string, payload []byte) error {
	return c.write(pktPublish<<4, append(appendString(nil, topic), payload...))
}

// publishAcked sends a message at QoS 1, and waits for the PUBACK that says the hub took it.
func (c *mqttConn) publishAcked(ctx context.Context, topic string, payload []byte) error {
	acked := make(chan error, 1)
	c.mu.Lock()
	if c.ended != nil {
		c.mu.Unlock()
		return c.ended
	}
	// The subscription took ID 1.
	c.nextID = max(c.nextID+1, 2)
	id := c.nextID
	c.waiting[id] = acked
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.waiting, id)
		c.mu.Unlock()
	}()
	body := binary.BigEndian.AppendUint16(appendString(nil, topic), id)
	if err := c.write(pktPublish<<4|0x02, append(body, payload...)); err != nil {
		return err
	}
	select {
	case err := <-acked:
		return err
	case <-ctx.Done():
		return fmt.Errorf("no PUBACK from the hub: %w", ctx.Err())
	}
}

// acked passes on a PUBACK to the publish waiting for it.
func (c *mqttConn) acked(body []byte) {
	if len(body) != 2 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Delivered once, so a PUBACK sent twice can't block on a full channel.
	id := binary.BigEndian.Uint16(body)
	if w, ok := c.waiting[id]; ok {
		w <- nil
		delete(c.waiting, id)
	}
}

// end fails every publish waiting for its PUBACK, and any to come.
func (c *mqttConn) end(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended == nil {
		c.ended = fmt.Errorf("the connection to the hub ended: %w", err)
	}
	// Each is still waiting, so its channel is empty.
	for id, w := range c.waiting {
		w <- c.ended
		delete(c.waiting, id)
	}
}

// run reads messages until the connection fails or ctx ends, and gives each to got. It pings as
// keep-alive says, and fails if the hub says nothing for half as long again.
func (c *mqttConn) run(ctx context.Context, got func(topic string, payload []byte)) (err error) {
	defer func() { c.end(err) }()
	stop := context.AfterFunc(ctx, func() {
		_ = c.write(pktDisconnect<<4, nil)
		_ = c.conn.Close()
	})
	defer stop()
	pinging, stopPinging := context.WithCancel(ctx)
	defer stopPinging()
	go func() {
		t := time.NewTicker(c.keepAlive)
		defer t.Stop()
		for {
			select {
			case <-pinging.Done():
				return
			case <-t.C:
				if c.write(pktPingreq<<4, nil) != nil {
					return
				}
			}
		}
	}()
	for {
		_ = c.conn.SetReadDeadline(time.Now().Add(c.keepAlive * 3 / 2))
		kind, body, err := c.read()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if kind>>4 == pktPuback {
			c.acked(body)
		}
		if kind>>4 != pktPublish {
			continue
		}
		topic, payload, id, err := parsePublish(kind, body)
		if err != nil {
			return err
		}
		if id != 0 {
			if err := c.write(pktPuback<<4, binary.BigEndian.AppendUint16(nil, id)); err != nil {
				return err
			}
		}
		got(topic, payload)
	}
}

// close ends the connection without a DISCONNECT.
func (c *mqttConn) close() error {
	c.end(errors.New("closed"))
	return c.conn.Close()
}

// parsePublish reads a PUBLISH's topic, payload and, at QoS 1 or 2, its packet ID.
func parsePublish(header byte, body []byte) (topic string, payload []byte, id uint16, err error) {
	if len(body) < 2 {
		return "", nil, 0, errors.New("a PUBLISH too short for its topic")
	}
	n := int(binary.BigEndian.Uint16(body))
	if len(body) < 2+n {
		return "", nil, 0, errors.New("a PUBLISH shorter than its topic")
	}
	topic, rest := string(body[2:2+n]), body[2+n:]
	if (header>>1)&0x03 != 0 {
		if len(rest) < 2 {
			return "", nil, 0, errors.New("a PUBLISH with no packet ID")
		}
		id, rest = binary.BigEndian.Uint16(rest), rest[2:]
	}
	return topic, rest, id, nil
}

// writeTimeout bounds one write, so a hub that stops reading can't hold the connection open.
const writeTimeout = 10 * time.Second

func (c *mqttConn) write(header byte, body []byte) error {
	pkt := append([]byte{header}, appendLength(nil, len(body))...)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := c.conn.Write(append(pkt, body...))
	return err
}

// read reads one packet: its fixed header's first byte, and the rest.
func (c *mqttConn) read() (byte, []byte, error) {
	header, err := c.r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	n, multiplier := 0, 1
	for i := 0; ; i++ {
		if i == 4 {
			return 0, nil, errors.New("a packet length over four bytes")
		}
		b, err := c.r.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		n += int(b&0x7F) * multiplier
		if b&0x80 == 0 {
			break
		}
		multiplier *= 128
	}
	if n > maxPacket {
		return 0, nil, fmt.Errorf("a packet of %d bytes, over %d", n, maxPacket)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(c.r, body); err != nil {
		return 0, nil, err
	}
	return header, body, nil
}

// appendString appends s as MQTT's two-byte length and its bytes.
func appendString(b []byte, s string) []byte {
	return append(binary.BigEndian.AppendUint16(b, uint16(len(s))), s...) //nolint:gosec // Topics and credentials are far under 64 KiB.
}

// appendLength appends n as MQTT's remaining length.
func appendLength(b []byte, n int) []byte {
	for {
		d := byte(n % 128)
		n /= 128
		if n > 0 {
			d |= 0x80
		}
		b = append(b, d)
		if n == 0 {
			return b
		}
	}
}
