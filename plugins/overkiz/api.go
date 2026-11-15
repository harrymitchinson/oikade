package overkiz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxResponse is the most of an answer the plugin reads. A setup with many devices is well inside it.
const maxResponse = 8 << 20

// client speaks Overkiz's enduser API, which TaHoma's local API and the cloud share: one base URL,
// and a way to authorize each request.
type client struct {
	http *http.Client
	// base is the API's root, ending in /enduserAPI.
	base string
	// authorize adds the credentials to a request.
	authorize func(*http.Request)
}

// apiDevice is a device in /setup/devices. A sub-device, such as a sensor inside a heater, is its own
// entry, its deviceURL ending in #2, #3 and so on.
type apiDevice struct {
	DeviceURL        string     `json:"deviceURL"`
	Label            string     `json:"label"`
	ControllableName string     `json:"controllableName"`
	Widget           string     `json:"widget"`
	UIClass          string     `json:"uiClass"`
	Available        bool       `json:"available"`
	Enabled          bool       `json:"enabled"`
	States           []apiState `json:"states"`
	Attributes       []apiState `json:"attributes"`
	Definition       struct {
		WidgetName string `json:"widgetName"`
		UIClass    string `json:"uiClass"`
	} `json:"definition"`
}

// widget is what kind of device it is. The cloud puts it in the definition, and the local API at the
// top too.
func (d apiDevice) widget() string {
	if d.Definition.WidgetName != "" {
		return d.Definition.WidgetName
	}
	return d.Widget
}

// attribute is one of its attributes as a string, or "".
func (d apiDevice) attribute(name string) string {
	for _, a := range d.Attributes {
		if a.Name == name {
			if s, ok := a.Value.(string); ok {
				return s
			}
		}
	}
	return ""
}

// apiState is a state or an attribute: a name, and a value whose JSON type follows its type.
type apiState struct {
	Name  string `json:"name"`
	Type  int    `json:"type"`
	Value any    `json:"value"`
}

// apiEvent is one event from a listener. Only the fields of the events the plugin uses are here.
type apiEvent struct {
	Name         string     `json:"name"`
	DeviceURL    string     `json:"deviceURL"`
	DeviceStates []apiState `json:"deviceStates"`
	ExecID       string     `json:"execId"`
	NewState     string     `json:"newState"`
	FailureType  string     `json:"failureType"`
}

// The events the plugin acts on.
const (
	eventStateChanged     = "DeviceStateChangedEvent"
	eventAvailable        = "DeviceAvailableEvent"
	eventUnavailable      = "DeviceUnavailableEvent"
	eventDisabled         = "DeviceDisabledEvent"
	eventCreated          = "DeviceCreatedEvent"
	eventRemoved          = "DeviceRemovedEvent"
	eventUpdated          = "DeviceUpdatedEvent"
	eventExecutionChanged = "ExecutionStateChangedEvent"
)

// An execution's states that end it.
const (
	execCompleted = "COMPLETED"
	execFailed    = "FAILED"
)

// apiCommand is one command to a device.
type apiCommand struct {
	Name       string `json:"name"`
	Parameters []any  `json:"parameters"`
}

// apiError is an error the API answered with: its HTTP status, and Overkiz's errorCode and message.
type apiError struct {
	Status  int
	Code    string `json:"errorCode"`
	Message string `json:"error"`
}

func (e *apiError) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("%s: %s (HTTP %d)", e.Code, e.Message, e.Status)
	case e.Message != "":
		return fmt.Sprintf("%s (HTTP %d)", e.Message, e.Status)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// What kind of failure an error is, which decides what the plugin does next.
type failure int

const (
	// unreachable is the hub or the cloud not answering, or answering with a server error: try
	// again soon.
	unreachable failure = iota
	// refused is the credentials refused: say so, and wait for new ones.
	refused
	// limited is too many requests: back off.
	limited
	// noListener is an event listener the server has forgotten: register another.
	noListener
	// rejected is a request the server won't take, such as a command a device doesn't have.
	rejected
)

// classify says what kind of failure err is. Overkiz says most in errorCode, and pyoverkiz's
// exceptions follow the same codes and messages.
func classify(err error) failure {
	var e *apiError
	if !errors.As(err, &e) {
		return unreachable
	}
	message := strings.ToLower(e.Message)
	switch {
	case strings.Contains(message, "no registered event listener"):
		return noListener
	case e.Status == http.StatusTooManyRequests, strings.Contains(message, "too many"):
		return limited
	case e.Status == http.StatusUnauthorized, e.Code == "AUTHENTICATION_ERROR", e.Code == "RESOURCE_ACCESS_DENIED",
		strings.Contains(message, "not authenticated"), strings.Contains(message, "bad credentials"):
		return refused
	case e.Status >= http.StatusInternalServerError:
		return unreachable
	}
	return rejected
}

// call sends a request to path under the base and decodes the answer into out, if it isn't nil.
func (c *client) call(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.authorize != nil {
		c.authorize(req)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &apiError{Status: resp.StatusCode}
		_ = json.Unmarshal(raw, e)
		return e
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	return nil
}

// devices reads every device in the setup, sub-devices included.
func (c *client) devices(ctx context.Context) ([]apiDevice, error) {
	var out []apiDevice
	return out, c.call(ctx, http.MethodGet, "/setup/devices", nil, &out)
}

// register starts an event listener and returns its ID.
func (c *client) register(ctx context.Context) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if err := c.call(ctx, http.MethodPost, "/events/register", nil, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("registering an event listener: no listener ID in the answer")
	}
	return out.ID, nil
}

// fetch takes the events a listener has gathered since it was last fetched.
func (c *client) fetch(ctx context.Context, listener string) ([]apiEvent, error) {
	var out []apiEvent
	return out, c.call(ctx, http.MethodPost, "/events/"+listener+"/fetch", nil, &out)
}

// unregister ends a listener.
func (c *client) unregister(ctx context.Context, listener string) error {
	return c.call(ctx, http.MethodPost, "/events/"+listener+"/unregister", nil, nil)
}

// execute runs commands on one device and returns the execution's ID once the server has taken it.
func (c *client) execute(ctx context.Context, deviceURL string, commands ...apiCommand) (string, error) {
	body := map[string]any{
		"label":   "Oikade",
		"actions": []map[string]any{{"deviceURL": deviceURL, "commands": commands}},
	}
	var out struct {
		ExecID string `json:"execId"`
	}
	if err := c.call(ctx, http.MethodPost, "/exec/apply", body, &out); err != nil {
		return "", err
	}
	return out.ExecID, nil
}
