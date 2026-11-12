package mdns

import "errors"

// Listener is an IPv4 socket on the mDNS port, joined to the group on every multicast interface,
// for something that asks and listens rather than answers. It shares the port with any responder
// on the host, this package's included.
type Listener struct {
	conn *mcastConn
}

// ListenMulticast opens a Listener.
func ListenMulticast() (*Listener, error) {
	ifaces, err := multicastInterfaces()
	if err != nil {
		return nil, err
	}
	if len(ifaces) == 0 {
		return nil, errors.New("mdns: no interface is up and can multicast")
	}
	conn, err := listen4(ifaces)
	if err != nil {
		return nil, err
	}
	return &Listener{conn: conn}, nil
}

// Read reads a packet into b, and returns its length. It fails once the Listener is closed.
func (l *Listener) Read(b []byte) (int, error) {
	n, _, _, err := l.conn.ReadPacket(b)
	return n, err
}

// Send sends b to the group on every interface the Listener joined it on. It tries them all, and
// fails only if it went out on none, with what went wrong on each. So an interface that can join the
// group but not send to it, such as a container bridge, doesn't fail every send.
func (l *Listener) Send(b []byte) error {
	errs := make([]error, 0, len(l.conn.ifaces))
	sent := false
	for index := range l.conn.ifaces {
		if err := l.conn.WriteMulticast(b, index); err != nil {
			errs = append(errs, err)
		} else {
			sent = true
		}
	}
	if sent {
		return nil
	}
	return errors.Join(errs...)
}

// Close closes the socket, which ends a Read.
func (l *Listener) Close() error { return l.conn.Close() }
