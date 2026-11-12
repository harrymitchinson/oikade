package ewelinklan

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5" //nolint:gosec // The protocol derives its AES key this way.
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
)

// Field names in a report, as a THR320D sends them. Both are strings, and a reading the probe can't
// take is "unavailable", as humidity is from a temperature-only probe.
const (
	fieldTemperature = "currentTemperature"
	fieldHumidity    = "currentHumidity"
)

// report is what one decrypted report says.
type report struct {
	// relay is whether the report mentions the relay, on what it says, and bad that what it says can't
	// be believed, which counts as not knowing.
	relay, on, bad bool
	// values are the sensor readings it carries, by capability.
	values map[string]float64
}

// kind is what a TXT type can do. A new type of device on the protocol is a new entry here.
type kind struct {
	// path is where a command is posted.
	path string
	// command is the body that switches the relay.
	command func(on bool) any
	// capabilities are what it reports, and sensors whether Config.Humidity can add to them.
	capabilities []string
	sensors      bool
	// read takes a report's fields.
	read func(params map[string]any) report
}

// kinds is keyed on the TXT record's type. Models that share a type are handled identically.
var kinds = map[string]kind{
	"plug": {
		path: "/zeroconf/switches",
		command: func(on bool) any {
			return map[string]any{"switches": []any{map[string]any{"outlet": 0, "switch": onOff(on)}}}
		},
		capabilities: []string{contractv1.On},
		read:         readSwitches,
	},
	"th_plug": {
		path: "/zeroconf/switch",
		command: func(on bool) any {
			return map[string]any{"deviceType": "normal", "mainSwitch": onOff(on), "switch": onOff(on)}
		},
		capabilities: []string{contractv1.On, contractv1.Temperature},
		sensors:      true,
		read:         readSensorSwitch,
	},
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// readSwitches reads a list of outlets, of which only outlet 0 is this relay. Conflicting reports of
// it are not believed.
func readSwitches(params map[string]any) report {
	switches, ok := params["switches"].([]any)
	if !ok {
		return report{}
	}
	var channel map[string]any
	for _, entry := range switches {
		fields, ok := entry.(map[string]any)
		if !ok || fields["outlet"] != float64(0) {
			continue
		}
		if channel != nil {
			return report{relay: true, bad: true}
		}
		channel = fields
	}
	if channel == nil {
		return report{}
	}
	return relayValue(channel["switch"])
}

// readSensorSwitch reads a relay with a probe. A reading that isn't a number, as when no probe is
// plugged in, isn't one.
func readSensorSwitch(params map[string]any) report {
	var r report
	if value, ok := params["switch"]; ok {
		r = relayValue(value)
	}
	r.values = map[string]float64{}
	if v, ok := reading(params[fieldTemperature], -270, 100); ok {
		r.values[contractv1.Temperature] = v
	}
	if v, ok := reading(params[fieldHumidity], 0, 100); ok {
		r.values[contractv1.Humidity] = v
	}
	return r
}

func relayValue(value any) report {
	switch value {
	case "on":
		return report{relay: true, on: true}
	case "off":
		return report{relay: true}
	}
	return report{relay: true, bad: true}
}

var decimal = regexp.MustCompile(`^[+-]?(?:\d+(?:\.\d*)?|\.\d+)$`)

// reading reads a number that a device may send as a string, within what it can measure.
func reading(value any, lowest, highest float64) (float64, bool) {
	var v float64
	switch n := value.(type) {
	case float64:
		v = n
	case string:
		if !decimal.MatchString(n) {
			return 0, false
		}
		parsed, err := strconv.ParseFloat(n, 64)
		if err != nil {
			return 0, false
		}
		v = parsed
	default:
		return 0, false
	}
	return v, v >= lowest && v <= highest
}

// errMalformed is a report that can't be decrypted by any key, which says nothing about the key.
var errMalformed = errors.New("malformed report")

// lanKey derives a device's AES key from its LAN key.
func lanKey(secret string) []byte {
	sum := md5.Sum([]byte(secret)) //nolint:gosec // The protocol derives its AES key this way.
	return sum[:]
}

// ciphertext is a TXT record's encrypted report, split over data1 to data4.
func ciphertext(txt map[string]string) (iv, data []byte, err error) {
	iv, err = base64.StdEncoding.DecodeString(txt["iv"])
	if err != nil || len(iv) != aes.BlockSize {
		return nil, nil, fmt.Errorf("%w: iv", errMalformed)
	}
	var joined strings.Builder
	for i := 1; i <= 4; i++ {
		chunk, ok := txt["data"+strconv.Itoa(i)]
		if !ok {
			break
		}
		joined.WriteString(chunk)
	}
	data, err = base64.StdEncoding.DecodeString(joined.String())
	if err != nil || len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, nil, fmt.Errorf("%w: data", errMalformed)
	}
	return iv, data, nil
}

// decrypt reads a report. An error that isn't errMalformed means the key is wrong, or the report
// was damaged in a way that looks the same.
func decrypt(iv, data, key []byte) (map[string]any, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, data)
	pad := int(plain[len(plain)-1])
	if pad == 0 || pad > aes.BlockSize || !bytes.Equal(plain[len(plain)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return nil, errors.New("bad padding")
	}
	var params map[string]any
	if err := json.Unmarshal(plain[:len(plain)-pad], &params); err != nil {
		return nil, errors.New("not JSON")
	}
	if params == nil {
		return nil, errors.New("not an object")
	}
	return params, nil
}

// encrypt seals a command, and returns its iv and data as a device expects them.
func encrypt(value any, key []byte) (iv, data string, err error) {
	plain, err := json.Marshal(value)
	if err != nil {
		return "", "", err
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	plain = append(plain, bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", "", err
	}
	nonce := make([]byte, aes.BlockSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", "", err
	}
	sealed := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, nonce).CryptBlocks(sealed, plain)
	return base64.StdEncoding.EncodeToString(nonce), base64.StdEncoding.EncodeToString(sealed), nil
}
