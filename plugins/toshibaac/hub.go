package toshibaac

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The hub's side of a device's connection, from Azure IoT Hub's MQTT support: the cloud calls the
// device's direct method smmobile, and the device answers each call.
const (
	hubPort       = "8883"
	hubAPIVersion = "2021-04-12"
	methodsFilter = "$iothub/methods/POST/#"
	methodsPrefix = "$iothub/methods/POST/"
	methodName    = "smmobile"

	cmdState     = "CMD_FCU_FROM_AC"
	cmdHeartbeat = "CMD_HEARTBEAT"
)

// sasToken is the token the cloud gives a registered device, for its hub.
type sasToken struct {
	raw string
	// host is the hub's name, and device the device ID the token is for.
	host, device string
	// expires is when it stops working, zero if it doesn't say.
	expires time.Time
}

// parseSAS reads a SharedAccessSignature's resource, sr=<hub>/devices/<device>, and its expiry, se.
func parseSAS(raw string) (sasToken, error) {
	fields, err := url.ParseQuery(strings.TrimPrefix(raw, "SharedAccessSignature "))
	if err != nil {
		return sasToken{}, fmt.Errorf("a SAS token that isn't key=value pairs: %w", err)
	}
	host, rest, ok := strings.Cut(fields.Get("sr"), "/devices/")
	if !ok || host == "" || rest == "" || strings.Contains(host, "/") {
		return sasToken{}, errors.New("a SAS token whose resource isn't <hub>/devices/<device>")
	}
	device, err := url.PathUnescape(rest)
	if err != nil {
		device = rest
	}
	t := sasToken{raw: raw, host: host, device: device}
	if se := fields.Get("se"); se != "" {
		seconds, err := strconv.ParseInt(se, 10, 64)
		if err != nil {
			return sasToken{}, fmt.Errorf("a SAS token whose expiry %q isn't a number", se)
		}
		t.expires = time.Unix(seconds, 0)
	}
	return t, nil
}

// hubOptions are where the hub is, which tests change.
type hubOptions struct {
	// address is host:port to dial, empty for the token's hub on 8883.
	address string
	// tls is the TLS config, nil for the system's roots and the hub's name.
	tls *tls.Config
}

// connectHub connects to the token's hub as its device, and subscribes to direct method calls.
func connectHub(ctx context.Context, tok sasToken, o hubOptions, keepAlive time.Duration) (*mqttConn, error) {
	address := o.address
	if address == "" {
		address = net.JoinHostPort(tok.host, hubPort)
	}
	config := o.tls
	if config == nil {
		config = &tls.Config{ServerName: tok.host, MinVersion: tls.VersionTLS12}
	}
	conn, err := (&tls.Dialer{Config: config}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	username := tok.host + "/" + tok.device + "/?api-version=" + hubAPIVersion
	c, err := mqttConnect(ctx, conn, tok.device, username, tok.raw, keepAlive)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := c.subscribe(ctx, methodsFilter); err != nil {
		_ = c.close()
		return nil, err
	}
	return c, nil
}

// methodCall is the payload of a call to smmobile.
type methodCall struct {
	Command  string          `json:"cmd"`
	SourceID string          `json:"sourceId"`
	Payload  json.RawMessage `json:"payload"`
}

// parseMethod reads a direct method call's name and request ID from its topic,
// $iothub/methods/POST/<name>/?$rid=<id>.
func parseMethod(topic string) (name, rid string, ok bool) {
	rest, ok := strings.CutPrefix(topic, methodsPrefix)
	if !ok {
		return "", "", false
	}
	name, query, ok := strings.Cut(rest, "/?")
	if !ok {
		return "", "", false
	}
	fields, err := url.ParseQuery(query)
	if err != nil || fields.Get("$rid") == "" {
		return "", "", false
	}
	return name, fields.Get("$rid"), true
}

// answerMethod answers a call, with status 0 and no payload, as the app's SDK does.
func answerMethod(c *mqttConn, rid string) error {
	return c.publish("$iothub/methods/res/0/?$rid="+url.QueryEscape(rid), []byte("null"))
}
