package ewelinklan

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/registry"
	"github.com/harrymitchinson/oikade/sdk/plugin"
)

const (
	// appIDSecret and appSecretSecret name the secrets that hold your eWeLink developer app's ID and
	// secret. A device's ID is hex, so no device's key can take either name.
	appIDSecret     = "ewelink-app-id"
	appSecretSecret = "ewelink-app-secret"
	// redirect is where the sign-in sends you back to, which oikade setup listens on. It's the
	// redirect URL to give the app.
	redirect = "http://127.0.0.1:8765/callback"
	// maxCloudAnswer is the most an answer from eWeLink's API may be.
	maxCloudAnswer = 4 << 20
)

// cloud is where eWeLink's sign-in page and API are, which the tests replace.
type cloud struct {
	signIn string
	// api is the API's address in a region: eu, us, as or cn.
	api    func(region string) string
	client *http.Client
}

var ewelink = cloud{
	signIn: "https://c2ccdn.coolkit.cc/oauth/index.html",
	api: func(region string) string {
		if region == "cn" {
			return "https://cn-apia.coolkit.cn"
		}
		return "https://" + region + "-apia.coolkit.cc"
	},
	client: &http.Client{Timeout: 15 * time.Second},
}

var regions = []string{"eu", "us", "as", "cn"}

// app is your eWeLink developer app.
type app struct{ id, secret string }

// sign is the Base64 HMAC-SHA256 of message, keyed with the app's secret, as eWeLink signs requests.
func (a app) sign(message []byte) string {
	mac := hmac.New(sha256.New, []byte(a.secret))
	mac.Write(message)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// cloudDevice is a device in your eWeLink account.
type cloudDevice struct{ id, key, name string }

// Setup implements plugin.Setup. It signs in to eWeLink through your browser, once, to fetch each
// device's LAN key into its secret, and changes only the keys that differ. The token it signs in
// with is never kept: the host reaches eWeLink only while setup runs.
func (b *bridge) Setup(ctx context.Context, _ *plugin.Plugin, s *plugin.Session) (string, error) {
	a, err := b.app(ctx, s)
	if err != nil {
		return "", err
	}
	token, region, err := b.signIn(ctx, s, a)
	if err != nil {
		return "", err
	}
	devices, err := b.cloud.devices(ctx, a, region, token)
	if err != nil {
		return "", err
	}
	if len(devices) == 0 {
		return "", errors.New("your eWeLink account has no devices: add them in the eWeLink app, then run setup again")
	}
	kept, unchanged := 0, 0
	for _, d := range devices {
		had, _, err := plugin.ReadSecret(b.env, d.id)
		if err != nil {
			return "", fmt.Errorf("could not read the LAN key of %s: %w", d.id, err)
		}
		if strings.TrimRight(had, "\r\n") == d.key {
			unchanged++
			continue
		}
		if err := s.KeepSecret(ctx, d.id, d.key); err != nil {
			return "", fmt.Errorf("could not keep the LAN key of %s: %w", d.id, err)
		}
		kept++
	}
	for _, d := range devices {
		if d.name == "" {
			continue
		}
		line := fmt.Sprintf("%s is %q in the eWeLink app: oikade devices name %s %s names it so", d.id, d.name, d.id,
			registry.Slug(d.name, d.id))
		if err := s.Say(ctx, line); err != nil {
			return "", err
		}
	}
	// The sweep reads the new keys, so the devices come online within seconds.
	return fmt.Sprintf("%s is set up: %s, %d already up to date", b.name(), plural(kept, "LAN key")+" kept", unchanged), nil
}

// app is your developer app, from its secrets, or asked for and kept once.
func (b *bridge) app(ctx context.Context, s *plugin.Session) (app, error) {
	id, haveID, err := b.readSecret(appIDSecret)
	if err != nil {
		return app{}, err
	}
	secret, haveSecret, err := b.readSecret(appSecretSecret)
	if err != nil {
		return app{}, err
	}
	if haveID && haveSecret {
		return app{id: id, secret: secret}, nil
	}
	if err := s.Say(ctx, "Make an app on the eWeLink developer platform, under Platform → App Management, "+
		"with the redirect URL "+redirect); err != nil {
		return app{}, err
	}
	if !haveID {
		if id, err = s.Ask(ctx, "Its app ID"); err != nil {
			return app{}, err
		}
		if id = strings.TrimSpace(id); id == "" {
			return app{}, errors.New("no app ID")
		}
		if err := s.KeepSecret(ctx, appIDSecret, id); err != nil {
			return app{}, fmt.Errorf("could not keep the app ID: %w", err)
		}
	}
	if !haveSecret {
		if secret, err = s.AskSecret(ctx, "Its app secret"); err != nil {
			return app{}, err
		}
		if secret = strings.TrimSpace(secret); secret == "" {
			return app{}, errors.New("no app secret")
		}
		if err := s.KeepSecret(ctx, appSecretSecret, secret); err != nil {
			return app{}, fmt.Errorf("could not keep the app secret: %w", err)
		}
	}
	return app{id: id, secret: secret}, nil
}

func (b *bridge) readSecret(name string) (string, bool, error) {
	value, ok, err := plugin.ReadSecret(b.env, name)
	if err != nil {
		return "", false, fmt.Errorf("could not read the secret %s: %w", name, err)
	}
	value = strings.TrimSpace(value)
	return value, ok && value != "", nil
}

// signIn opens eWeLink's sign-in page and swaps the code it sends you back with for a token, at once,
// since the code lasts 30 seconds.
func (b *bridge) signIn(ctx context.Context, s *plugin.Session, a app) (token, region string, err error) {
	seq := strconv.FormatInt(time.Now().UnixMilli(), 10)
	nonce := rand.Text()[:8]
	state := rand.Text()
	page := b.cloud.signIn + "?" + url.Values{
		"clientId":      {a.id},
		"seq":           {seq},
		"authorization": {a.sign([]byte(a.id + "_" + seq))},
		"redirectUrl":   {redirect},
		"grantType":     {"authorization_code"},
		"state":         {state},
		"nonce":         {nonce},
		"showQRCode":    {"false"},
	}.Encode()
	if err := s.Say(ctx, "Sign in with your eWeLink account"); err != nil {
		return "", "", err
	}
	q, err := s.Open(ctx, page, redirect)
	if err != nil {
		return "", "", err
	}
	switch region = q.Get("region"); {
	case q.Get("state") != state:
		return "", "", errors.New("the sign-in sent back another sign-in's answer: run setup again")
	case q.Get("code") == "":
		return "", "", fmt.Errorf("the sign-in sent back no code%s: check the app's ID, secret and redirect URL, "+
			"and change them with oikade secret set %s and %s", detail(q), appIDSecret, appSecretSecret)
	case !slices.Contains(regions, region):
		return "", "", fmt.Errorf("the sign-in sent back the region %q, which isn't one of %s", region, strings.Join(regions, ", "))
	}
	token, err = b.cloud.token(ctx, a, region, q.Get("code"), nonce)
	return token, region, err
}

// detail is what a redirect says went wrong, if anything.
func detail(q url.Values) string {
	for _, key := range []string{"error_description", "error", "msg"} {
		if v := q.Get(key); v != "" {
			return fmt.Sprintf(" (%s)", v)
		}
	}
	return ""
}

// answer is the envelope of every answer from eWeLink's API.
type answer struct {
	Error int             `json:"error"`
	Msg   string          `json:"msg"`
	Data  json.RawMessage `json:"data"`
}

func (c cloud) token(ctx context.Context, a app, region, code, nonce string) (string, error) {
	body, err := json.Marshal(map[string]string{"code": code, "redirectUrl": redirect, "grantType": "authorization_code"})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api(region)+"/v2/user/oauth/token", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CK-Appid", a.id)
	req.Header.Set("X-CK-Nonce", nonce)
	req.Header.Set("Authorization", "Sign "+a.sign(body))
	var data struct {
		AccessToken string `json:"accessToken"`
	}
	if err := c.call(req, "sign in", &data); err != nil {
		return "", err
	}
	if data.AccessToken == "" {
		return "", errors.New("eWeLink signed you in, but sent no token")
	}
	return data.AccessToken, nil
}

func (c cloud) devices(ctx context.Context, a app, region, token string) ([]cloudDevice, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.api(region)+"/v2/device/thing?num=0", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-CK-Appid", a.id)
	req.Header.Set("Authorization", "Bearer "+token)
	var data struct {
		ThingList []struct {
			ItemData struct {
				DeviceID  string `json:"deviceid"`
				DeviceKey string `json:"devicekey"`
				Name      string `json:"name"`
			} `json:"itemData"`
		} `json:"thingList"`
	}
	if err := c.call(req, "list your devices", &data); err != nil {
		return nil, err
	}
	var out []cloudDevice
	for _, t := range data.ThingList {
		d := t.ItemData
		// A group, or a device shared without its key, has none to fetch.
		id := strings.ToLower(strings.TrimSpace(d.DeviceID))
		if id == "" || d.DeviceKey == "" || !contractv1.ValidSecretName(id) {
			continue
		}
		out = append(out, cloudDevice{id: id, key: d.DeviceKey, name: strings.TrimSpace(d.Name)})
	}
	slices.SortFunc(out, func(x, y cloudDevice) int { return strings.Compare(x.id, y.id) })
	return out, nil
}

// call sends a request to eWeLink's API and reads its answer's data into into. Neither the token nor
// the keys in an answer go into an error.
func (c cloud) call(req *http.Request, doing string, into any) error {
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach eWeLink to %s: %w", doing, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCloudAnswer+1))
	if err != nil {
		return fmt.Errorf("could not read eWeLink's answer when trying to %s: %w", doing, err)
	}
	if len(raw) > maxCloudAnswer {
		return fmt.Errorf("eWeLink sent too long an answer when asked to %s", doing)
	}
	var a answer
	if err := json.Unmarshal(raw, &a); err != nil {
		return fmt.Errorf("eWeLink answered %s, which isn't JSON, when asked to %s", resp.Status, doing)
	}
	if a.Error != 0 {
		return fmt.Errorf("eWeLink refused to %s: %s (error %d)", doing, a.Msg, a.Error)
	}
	if err := json.Unmarshal(a.Data, into); err != nil {
		return fmt.Errorf("could not read eWeLink's answer when trying to %s: %w", doing, err)
	}
	return nil
}

// needsSetupLocked is whether a device heard has no LAN key. Call with mu held.
func (b *bridge) needsSetupLocked() bool {
	for _, d := range b.devices {
		if d.key == nil {
			return true
		}
	}
	return false
}

// name is the plugin's name, as oikade setup names it.
func (b *bridge) name() string {
	if name := b.env.Root.Plugin(); name != "" {
		return name
	}
	return "ewelink-lan"
}

func plural(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return fmt.Sprintf("%d %ss", n, thing)
}
