package mdns

import mdnsnet "oikade.dev/internal/mdns"

// The responder's sockets are internal/mdns's, which knows nothing of Matter.

// Port is the mDNS port (RFC 6762).
const Port = mdnsnet.Port

const maxPacket = mdnsnet.MaxPacket

type packetConn = mdnsnet.PacketConn
