package mdns

import mdnsnet "github.com/harrymitchinson/oikade/internal/mdns"

// The responder's sockets are internal/mdns's, which knows nothing of Matter.

// Port is the mDNS port (RFC 6762).
const Port = mdnsnet.Port

const maxPacket = mdnsnet.MaxPacket

type packetConn = mdnsnet.PacketConn

var (
	listen4             = mdnsnet.Listen4
	listen6             = mdnsnet.Listen6
	multicastInterfaces = mdnsnet.MulticastInterfaces
	group4              = mdnsnet.Group4
)
