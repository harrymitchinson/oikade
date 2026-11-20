package overkiz

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Brand is whose Cozytouch app the account is in. They share one login and its clouds.
type Brand string

// The brands on Cozytouch.
const (
	Atlantic Brand = "Atlantic"
	Thermor  Brand = "Thermor"
	Sauter   Brand = "Sauter"
)

// Cozytouch is an account in Atlantic's, Thermor's or Sauter's Cozytouch app, over the cloud: those
// devices have no local API. Its devices are on Overkiz, on Atlantic's own cloud, Magellan, or on
// both, and the plugin serves them all. The APIs aren't published, and work with a client ID from
// Atlantic's app, which Atlantic can change.
type Cozytouch struct {
	Brand Brand
	// Username is the account's email address, such as oikade.Var("COZYTOUCH_USER").
	Username string
	// Password is the account's password, such as oikade.Var("COZYTOUCH_PASSWORD").
	Password string

	// signer shares one login between the account's two clouds.
	signer *signer
	// The tests' servers, in place of the real ones.
	atlanticAPI, endpoint string
	transport             http.RoundTripper
}

// The Cozytouch cloud, as pyoverkiz has it: Atlantic's API for the login, and Overkiz's server for
// the rest. cozytouchClientID is the Basic credential Atlantic's app logs in with.
const (
	atlanticAPI       = "https://apis.groupe-atlantic.com"
	cozytouchEndpoint = "https://ha110-1.overkiz.com/enduser-mobile-web/enduserAPI"
	cozytouchClientID = "Q3RfMUpWeVRtSUxYOEllZkE3YVVOQmpGblpVYToyRWNORHpfZHkzNDJVSnFvMlo3cFNKTnZVdjBh"
)

// cloudTiming keeps well inside the cloud's limits, which pyoverkiz gives as one event fetch a
// second and one full device read a day per session. The setup is read at login and when a device
// changes, so that holds for a home that doesn't change hourly. Magellan, which has no events, is
// read once a minute: its setup gives a rateLimit of 30, which that's inside whether it's seconds
// or requests a minute.
var cloudTiming = timing{
	poll: 30 * time.Second, busy: 2 * time.Second, read: time.Minute, retry: 30 * time.Second, retryMax: 5 * time.Minute,
	limited: 10 * time.Minute, refused: time.Hour,
	request: 15 * time.Second, set: 4 * time.Second, execution: 2 * time.Minute,
}

func (Cozytouch) credentialField() string { return "overkiz.Cozytouch's Password" }

func (c Cozytouch) describe() string { return string(c.Brand) + "'s Cozytouch cloud" }

func (c Cozytouch) timing() timing { return cloudTiming }

func (c Cozytouch) manufacturer() string { return string(c.Brand) }

func (c Cozytouch) expires() bool { return true }

func (c Cozytouch) check() error {
	switch {
	case c.Brand != Atlantic && c.Brand != Thermor && c.Brand != Sauter:
		return fmt.Errorf("overkiz.Cozytouch has Brand %q: it's overkiz.Atlantic, overkiz.Thermor or overkiz.Sauter", c.Brand)
	case c.Username == "":
		return errors.New("overkiz.Cozytouch has no Username")
	case c.Password == "":
		return errors.New("overkiz.Cozytouch has no Password")
	}
	return nil
}

// connect logs in, in three calls: a password grant from Atlantic, its exchange for a JWT, and an
// Overkiz session for that, which a cookie keeps.
func (c Cozytouch) connect(ctx context.Context) (*client, error) {
	api, endpoint := cmp.Or(c.atlanticAPI, atlanticAPI), cmp.Or(c.endpoint, cozytouchEndpoint)
	jar, _ := cookiejar.New(nil)
	httpc := &http.Client{Jar: jar, Timeout: cloudTiming.request, Transport: c.transport}
	// A token both clouds have used may have ended, so one refused for the JWT is dropped, and a new
	// one asked for once: only a new token refused is a refused login.
	var jwt string
	for fresh := false; ; fresh = true {
		token, err := c.token(ctx)
		if err != nil {
			return nil, err
		}
		status, body, err := send(ctx, httpc, http.MethodGet, api+"/magellan/accounts/jwt", nil, "Bearer "+token)
		if err != nil {
			return nil, err
		}
		jwt = strings.Trim(strings.TrimSpace(string(body)), `"`)
		switch {
		case status == http.StatusUnauthorized:
			c.forget(token)
			if !fresh {
				continue
			}
			return nil, overkizError(status, body)
		case status < 200 || status > 299:
			return nil, overkizError(status, body)
		case jwt == "":
			return nil, &apiError{Status: status, Message: "Atlantic gave no JWT"}
		}
		break
	}

	status, body, err := send(ctx, httpc, http.MethodPost, endpoint+"/login", url.Values{"jwt": {jwt}}, "")
	if err != nil {
		return nil, err
	}
	var login struct {
		Success bool `json:"success"`
	}
	if status < 200 || status > 299 || (status == http.StatusOK && json.Unmarshal(body, &login) == nil && !login.Success) {
		return nil, overkizError(status, body)
	}
	return &client{http: httpc, base: endpoint}, nil
}

// token is Atlantic's token for the account, which both clouds take: the signer's, if the account
// has one, so the two share a login.
func (c Cozytouch) token(ctx context.Context) (string, error) {
	if c.signer != nil {
		return c.signer.token(ctx, c)
	}
	token, _, err := c.grant(ctx)
	return token, err
}

// forget drops a token a cloud refused, so the next login asks for another.
func (c Cozytouch) forget(token string) {
	if c.signer != nil {
		c.signer.forget(token)
	}
}

// grant asks Atlantic for a token with the account's password, and says how long it lasts.
func (c Cozytouch) grant(ctx context.Context) (string, time.Duration, error) {
	httpc := &http.Client{Timeout: cloudTiming.request, Transport: c.transport}
	status, body, err := send(ctx, httpc, http.MethodPost, cmp.Or(c.atlanticAPI, atlanticAPI)+"/token", url.Values{
		"grant_type": {"password"}, "username": {"GA-PRIVATEPERSON/" + c.Username}, "password": {c.Password},
	}, "Basic "+cozytouchClientID)
	if err != nil {
		return "", 0, err
	}
	var grant struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &grant)
	switch {
	case grant.Error == "invalid_grant":
		// Atlantic says a password is wrong this way. The message is its own.
		return "", 0, &apiError{Status: status, Code: codeAuthentication, Message: "Bad credentials: " + grant.Description}
	case grant.Error != "" && status >= 400 && status <= 499:
		// Any other refusal, such as invalid_client once Atlantic changes its app's client ID, won't
		// pass by trying again soon either.
		return "", 0, &apiError{Status: status, Code: codeAuthentication, Message: "Atlantic refused the login: " + grant.Error}
	case status < 200 || status > 299:
		return "", 0, overkizError(status, body)
	case grant.AccessToken == "":
		return "", 0, &apiError{Status: status, Message: "Atlantic gave no token"}
	}
	return grant.AccessToken, time.Duration(grant.ExpiresIn) * time.Second, nil
}

// signer keeps one Atlantic token for both of an account's clouds, and a refused password, so the
// two log in once between them: too many failed logins can lock the account.
type signer struct {
	// hold is how long a refused password is answered without asking again.
	hold time.Duration

	// mu is held through a grant, so a cloud that asks meanwhile waits for it.
	mu      sync.Mutex
	current string
	expires time.Time
	refusal error
	refused time.Time
}

// tokenMargin is how long before Atlantic says a token ends that it's asked for another.
const tokenMargin = 10 * time.Minute

func (s *signer) token(ctx context.Context, c Cozytouch) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	switch {
	case s.current != "" && now.Before(s.expires):
		return s.current, nil
	case s.refusal != nil && now.Sub(s.refused) < s.hold:
		return "", s.refusal
	}
	token, life, err := c.grant(ctx)
	s.current, s.refusal = "", nil
	switch {
	case err != nil && classify(err) == refused:
		s.refusal, s.refused = err, now
		return "", err
	case err != nil:
		return "", err
	}
	// A token whose life Atlantic doesn't say is kept for an hour, or until a cloud refuses it.
	if life <= 0 {
		life = time.Hour
	}
	s.current, s.expires = token, now.Add(life-min(tokenMargin, life/2))
	return token, nil
}

func (s *signer) forget(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == token {
		s.current = ""
	}
}

// send makes one login request: a form, if there is one, and the Authorization header, if any.
func send(ctx context.Context, httpc *http.Client, method, to string, form url.Values, authorization string) (int, []byte, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, to, body)
	if err != nil {
		return 0, nil, err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	return resp.StatusCode, raw, err
}

// overkizError is an answer that failed, with Overkiz's errorCode and message if it has them.
func overkizError(status int, body []byte) error {
	e := &apiError{Status: status}
	if json.Unmarshal(body, e) != nil && strings.Contains(string(body), "is down for maintenance") {
		e.Message = "down for maintenance"
	}
	if status >= 200 && status <= 299 && e.Message == "" {
		e.Message = "login refused"
		e.Code = codeAuthentication
	}
	return e
}
