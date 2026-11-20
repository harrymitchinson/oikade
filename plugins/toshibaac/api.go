package toshibaac

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The Home AC Control app's API, as KaSroka/Toshiba-AC-control has it, with the headers
// vmvelev/Toshiba-AC-control found the cloud wants since 2026-07: a Device-ID on every request, as
// the app sends its Android ID, and no User-Agent. Without the Device-ID a login is answered 429, and
// a browser's User-Agent is refused with 403.
const (
	apiBase = "https://mobileapi.toshibahomeaccontrols.com"

	loginPath    = "/api/Consumer/Login"
	registerPath = "/api/Consumer/RegisterMobileDevice"
	mappingPath  = "/api/AC/GetConsumerACMapping"
	statePath    = "/api/AC/GetCurrentACState"

	// badPassword is the StatusCode of a refused login.
	badPassword = "InvalidUserNameorPassword"
)

// maxResponse bounds an answer the plugin reads.
const maxResponse = 4 << 20

// What went wrong, as the plugin acts on it. Anything else is the cloud not answering, which it
// tries again with backoff.
var (
	// errRefused is a username or password the cloud refused: trying again soon could lock the account.
	errRefused = errors.New("refused the login")
	// errLimited is the cloud saying there were too many requests: its firewall answers 403, and its
	// login 429. Its statusError says how long the cloud asked the plugin to wait, if it did.
	errLimited = errors.New("says there were too many requests")
	// errRenew is a session that has to start again: a token that ran out.
	errRenew = errors.New("the session ended")
)

// api is a logged-in session with the cloud.
type api struct {
	http          *http.Client
	base          string
	device        string
	authorization string
	consumer      string
}

// unit is an indoor unit, as the cloud lists it.
type unit struct {
	ID       string `json:"Id"`
	UniqueID string `json:"DeviceUniqueId"`
	Name     string `json:"Name"`
	State    string `json:"ACStateData"`
	Merit    string `json:"MeritFeature"`
}

// answer is every answer's envelope.
type answer struct {
	IsSuccess  bool            `json:"IsSuccess"`
	Message    string          `json:"Message"`
	StatusCode string          `json:"StatusCode"`
	ResObj     json.RawMessage `json:"ResObj"`
}

// login logs in with the account's username and password, as the device with the Device-ID device.
func login(ctx context.Context, httpc *http.Client, base, device, username, password string) (*api, error) {
	a := &api{http: httpc, base: base, device: device}
	var res struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ConsumerID  string `json:"consumerId"`
	}
	err := a.call(ctx, http.MethodPost, loginPath, nil, map[string]string{"Username": username, "Password": password}, &res)
	var status *statusError
	if errors.As(err, &status) && status.code == http.StatusUnauthorized {
		return nil, fmt.Errorf("%w: %w", errRefused, err)
	}
	if err != nil {
		return nil, err
	}
	if res.AccessToken == "" || res.ConsumerID == "" {
		return nil, errors.New("the login gave no token")
	}
	a.authorization, a.consumer = res.TokenType+" "+res.AccessToken, res.ConsumerID
	return a, nil
}

// register registers the plugin as a mobile device, and returns the SAS token it connects to the
// hub with.
func (a *api) register(ctx context.Context, deviceID, username string) (string, error) {
	var res struct {
		SasToken string `json:"SasToken"`
	}
	if err := a.call(ctx, http.MethodPost, registerPath, nil,
		map[string]string{"DeviceID": deviceID, "DeviceType": "1", "Username": username}, &res); err != nil {
		return "", err
	}
	if res.SasToken == "" {
		return "", errors.New("registering gave no SAS token")
	}
	return res.SasToken, nil
}

// units lists the account's indoor units.
func (a *api) units(ctx context.Context) ([]unit, error) {
	var groups []struct {
		ACList []unit `json:"ACList"`
	}
	if err := a.call(ctx, http.MethodGet, mappingPath, url.Values{"consumerId": {a.consumer}}, nil, &groups); err != nil {
		return nil, err
	}
	var units []unit
	for _, g := range groups {
		units = append(units, g.ACList...)
	}
	return units, nil
}

// state reads a unit's state, by its cloud ID.
func (a *api) state(ctx context.Context, id string) (string, error) {
	var res struct {
		State *string `json:"ACStateData"`
	}
	if err := a.call(ctx, http.MethodGet, statePath, url.Values{"ACId": {id}, "consumerId": {a.consumer}}, nil, &res); err != nil {
		return "", err
	}
	if res.State == nil {
		return "", errors.New("a state answer with no ACStateData")
	}
	return *res.State, nil
}

// statusError is an answer other than 200.
type statusError struct {
	path string
	code int
	// wait is how long the answer asked the plugin to wait before trying again, if asked.
	wait  time.Duration
	asked bool
}

func (e *statusError) Error() string { return fmt.Sprintf("HTTP %d from %s", e.code, e.path) }

// call makes one request, and decodes its ResObj into res.
func (a *api) call(ctx context.Context, method, path string, query url.Values, body, res any) error {
	to := a.base + path
	if query != nil {
		to += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, to, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Device-ID", a.device)
	req.Header.Set("User-Agent", "") // Empty sends none, rather than Go's.
	if a.authorization != "" {
		req.Header.Set("Authorization", a.authorization)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		wait, asked := askedWait(resp.Header, raw, time.Now())
		return fmt.Errorf("%w: %w", errLimited, &statusError{path: path, code: resp.StatusCode, wait: wait, asked: asked})
	case resp.StatusCode == http.StatusUnauthorized && a.authorization != "":
		// The access token ran out: logging in again gets another.
		return fmt.Errorf("%w: %w", errRenew, &statusError{path: path, code: resp.StatusCode})
	case resp.StatusCode != http.StatusOK:
		return &statusError{path: path, code: resp.StatusCode}
	}
	var envelope answer
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("an answer from %s that isn't JSON: %w", path, err)
	}
	if !envelope.IsSuccess {
		if envelope.StatusCode == badPassword {
			return fmt.Errorf("%w: %s", errRefused, envelope.Message)
		}
		return fmt.Errorf("%s failed: %s (%s)", path, envelope.Message, envelope.StatusCode)
	}
	if err := json.Unmarshal(envelope.ResObj, res); err != nil {
		return fmt.Errorf("an answer from %s with a ResObj the plugin can't read: %w", path, err)
	}
	return nil
}

// tryAgain is the wait a 429's body says, as the login's did in OIK-327:
// {"message":"Too many requests. Try again in 60 seconds."}.
var tryAgain = regexp.MustCompile(`(?i)try again in (\d+) seconds?`)

// askedWait is how long an answer asks the plugin to wait before trying again: its Retry-After, in
// seconds or as a date, or else the "Try again in 60 seconds" in its body. asked is false if it says
// neither.
func askedWait(h http.Header, body []byte, now time.Time) (wait time.Duration, asked bool) {
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if s, err := strconv.ParseUint(v, 10, 32); err == nil {
			return time.Duration(s) * time.Second, true
		}
		if t, err := http.ParseTime(v); err == nil {
			return max(t.Sub(now), 0), true
		}
	}
	if m := tryAgain.FindSubmatch(body); m != nil {
		if s, err := strconv.ParseUint(string(m[1]), 10, 32); err == nil {
			return time.Duration(s) * time.Second, true
		}
	}
	return 0, false
}
