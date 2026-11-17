// Package mdns is the home network's multicast DNS (RFC 6762): sockets on port 5353 that share it
// with any responder on the host, a Listener for something that asks and listens, and LookupHost and
// DialContext, which find a host in .local. It knows nothing of Matter, so oikade links it too. The
// Matter responder is internal/matter/mdns, built on it.
package mdns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// Port is the mDNS port (RFC 6762).
const Port = 5353

// The mDNS multicast groups.
var (
	Group4 = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: Port}
	group6 = &net.UDPAddr{IP: net.ParseIP("ff02::fb"), Port: Port}
)

// MaxPacket is the largest mDNS message we read (RFC 6762, 17).
const MaxPacket = 9000

// PacketConn is one address family's socket on port 5353, joined to the mDNS group on some
// interfaces.
type PacketConn interface {
	// ReadPacket reads a packet, and returns the interface it came in on and its sender.
	ReadPacket(b []byte) (n, ifIndex int, src *net.UDPAddr, err error)
	// WriteMulticast sends b to the mDNS group on an interface. It does nothing if the conn
	// hasn't joined the group there.
	WriteMulticast(b []byte, ifIndex int) error
	// WriteUnicast sends b to dst.
	WriteUnicast(b []byte, dst *net.UDPAddr) error
	Close() error
}

// mcastConn is a PacketConn on a real socket.
type mcastConn struct {
	pc     net.PacketConn
	group  *net.UDPAddr
	ifaces map[int]*net.Interface // that it joined the group on
	read   func(b []byte) (n, ifIndex int, src net.Addr, err error)
	setIf  func(*net.Interface) error

	mu sync.Mutex // the multicast interface is per socket
}

func (c *mcastConn) ReadPacket(b []byte) (int, int, *net.UDPAddr, error) {
	n, ifIndex, src, err := c.read(b)
	ua, _ := src.(*net.UDPAddr)
	if err == nil && ua == nil {
		err = fmt.Errorf("mdns: packet from %v, not a UDP address", src)
	}
	return n, ifIndex, ua, err
}

func (c *mcastConn) WriteMulticast(b []byte, ifIndex int) error {
	ifi, ok := c.ifaces[ifIndex]
	if !ok {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.setIf(ifi); err != nil {
		return err
	}
	_, err := c.pc.WriteTo(b, c.group)
	return err
}

func (c *mcastConn) WriteUnicast(b []byte, dst *net.UDPAddr) error {
	_, err := c.pc.WriteTo(b, dst)
	return err
}

func (c *mcastConn) Close() error { return c.pc.Close() }

// listenUDP binds port 5353 on network ("udp4" or "udp6"), sharing it with any other responder
// on the host, such as mDNSResponder or Avahi.
func listenUDP(network string) (net.PacketConn, error) {
	lc := net.ListenConfig{Control: reusePort}
	return lc.ListenPacket(context.Background(), network, fmt.Sprintf(":%d", Port))
}

// Listen4 joins 224.0.0.251 on ifaces.
func Listen4(ifaces []net.Interface) (PacketConn, error) {
	c, err := listen4(ifaces)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Listen6 joins ff02::fb on ifaces.
func Listen6(ifaces []net.Interface) (PacketConn, error) {
	c, err := listen6(ifaces)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func listen4(ifaces []net.Interface) (*mcastConn, error) {
	pc, err := listenUDP("udp4")
	if err != nil {
		return nil, err
	}
	p := ipv4.NewPacketConn(pc)
	c := &mcastConn{pc: pc, group: Group4, ifaces: map[int]*net.Interface{}, setIf: p.SetMulticastInterface}
	var errs []error
	for i := range ifaces {
		if err := p.JoinGroup(&ifaces[i], Group4); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ifaces[i].Name, err))
			continue
		}
		c.ifaces[ifaces[i].Index] = &ifaces[i]
	}
	if len(c.ifaces) == 0 {
		_ = pc.Close()
		return nil, fmt.Errorf("mdns: joined the IPv4 group on no interface: %w", errors.Join(errs...))
	}
	if err := errors.Join(
		p.SetControlMessage(ipv4.FlagInterface, true),
		p.SetMulticastTTL(255),
		p.SetMulticastLoopback(true), // so that browsers on this host find us
	); err != nil {
		_ = pc.Close()
		return nil, err
	}
	c.read = func(b []byte) (int, int, net.Addr, error) {
		n, cm, src, err := p.ReadFrom(b)
		if cm == nil {
			return n, 0, src, err
		}
		return n, cm.IfIndex, src, err
	}
	return c, nil
}

func listen6(ifaces []net.Interface) (*mcastConn, error) {
	pc, err := listenUDP("udp6")
	if err != nil {
		return nil, err
	}
	p := ipv6.NewPacketConn(pc)
	c := &mcastConn{pc: pc, group: group6, ifaces: map[int]*net.Interface{}, setIf: p.SetMulticastInterface}
	var errs []error
	for i := range ifaces {
		if err := p.JoinGroup(&ifaces[i], group6); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ifaces[i].Name, err))
			continue
		}
		c.ifaces[ifaces[i].Index] = &ifaces[i]
	}
	if len(c.ifaces) == 0 {
		_ = pc.Close()
		return nil, fmt.Errorf("mdns: joined the IPv6 group on no interface: %w", errors.Join(errs...))
	}
	if err := errors.Join(
		p.SetControlMessage(ipv6.FlagInterface, true),
		p.SetMulticastHopLimit(255),
		p.SetMulticastLoopback(true),
	); err != nil {
		_ = pc.Close()
		return nil, err
	}
	c.read = func(b []byte) (int, int, net.Addr, error) {
		n, cm, src, err := p.ReadFrom(b)
		if cm == nil {
			return n, 0, src, err
		}
		return n, cm.IfIndex, src, err
	}
	return c, nil
}

// MulticastInterfaces returns the interfaces that are up and multicast-capable, except loopback.
func MulticastInterfaces() ([]net.Interface, error) {
	all, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []net.Interface
	for _, ifi := range all {
		if ifi.Flags&net.FlagUp != 0 && ifi.Flags&net.FlagMulticast != 0 && ifi.Flags&net.FlagLoopback == 0 {
			out = append(out, ifi)
		}
	}
	return out, nil
}
