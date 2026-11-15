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
	"time"
)

// Brand is whose Cozytouch app the account is in. They share one cloud.
type Brand string

// The brands on Cozytouch.
const (
	Atlantic Brand = "Atlantic"
	Thermor  Brand = "Thermor"
	Sauter   Brand = "Sauter"
)

// Cozytouch is an account in Atlantic's, Thermor's or Sauter's Cozytouch app, over the cloud: those
// devices have no local API. The API isn't published, and works with a client ID from Atlantic's
// app, which Atlantic can change.
type Cozytouch struct {
	Brand Brand
	// Username is the account's email address.
	Username string
	// PasswordSecret is the secret that holds the account's password.
	PasswordSecret string

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
// changes, so that holds for a home that doesn't change hourly.
var cloudTiming = timing{
	poll: 30 * time.Second, busy: 2 * time.Second, retry: 30 * time.Second, retryMax: 5 * time.Minute,
	limited: 10 * time.Minute, refused: time.Hour, secret: 5 * time.Second,
	request: 15 * time.Second, set: 4 * time.Second, execution: 2 * time.Minute,
}

func (c Cozytouch) secret() string { return c.PasswordSecret }

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
	}
	return nil
}

// connect logs in, in three calls: a password grant from Atlantic, its exchange for a JWT, and an
// Overkiz session for that, which a cookie keeps.
func (c Cozytouch) connect(ctx context.Context, password string) (*client, error) {
	api, endpoint := cmp.Or(c.atlanticAPI, atlanticAPI), cmp.Or(c.endpoint, cozytouchEndpoint)
	jar, _ := cookiejar.New(nil)
	httpc := &http.Client{Jar: jar, Timeout: cloudTiming.request, Transport: c.transport}

	status, body, err := send(ctx, httpc, http.MethodPost, api+"/token", url.Values{
		"grant_type": {"password"}, "username": {"GA-PRIVATEPERSON/" + c.Username}, "password": {password},
	}, "Basic "+cozytouchClientID)
	if err != nil {
		return nil, err
	}
	var grant struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &grant)
	switch {
	case grant.Error == "invalid_grant":
		// Atlantic says a password is wrong this way. The message is its own.
		return nil, &apiError{Status: status, Code: codeAuthentication, Message: "Bad credentials: " + grant.Description}
	case grant.Error != "" && status >= 400 && status <= 499:
		// Any other refusal, such as invalid_client once Atlantic changes its app's client ID, won't
		// pass by trying again soon either.
		return nil, &apiError{Status: status, Code: codeAuthentication, Message: "Atlantic refused the login: " + grant.Error}
	case status < 200 || status > 299:
		return nil, overkizError(status, body)
	case grant.AccessToken == "":
		return nil, &apiError{Status: status, Message: "Atlantic gave no token"}
	}

	status, body, err = send(ctx, httpc, http.MethodGet, api+"/magellan/accounts/jwt", nil, "Bearer "+grant.AccessToken)
	if err != nil {
		return nil, err
	}
	jwt := strings.Trim(strings.TrimSpace(string(body)), `"`)
	switch {
	case status < 200 || status > 299:
		return nil, overkizError(status, body)
	case jwt == "":
		return nil, &apiError{Status: status, Message: "Atlantic gave no JWT"}
	}

	status, body, err = send(ctx, httpc, http.MethodPost, endpoint+"/login", url.Values{"jwt": {jwt}}, "")
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
