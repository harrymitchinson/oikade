package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/store"
)

// requestTimeout is how long a caller has to say what it wants.
const requestTimeout = 5 * time.Second

// writeTimeout bounds one answer, or one event to a tail, so a caller that stopped reading can't
// hold it open.
const writeTimeout = 5 * time.Second

// idleTimeout closes a kept-alive connection that's doing nothing.
const idleTimeout = 30 * time.Second

// maxRequest bounds a request's body. The largest is a restore's registry.
const maxRequest = 16 << 20

// maxPublicRequest bounds the body of a call that needs no token, such as a login.
const maxPublicRequest = 4 << 10

// apiServer answers the oikade command. A call outlives its connection: a deploy that has begun
// finishes even if the command that asked goes away.
type apiServer struct {
	log     *slog.Logger
	ctx     context.Context
	deploys *deployer
	srv     *server
	devices *inventory
	// bridge is nil on a host with Matter off.
	bridge *matterBridge
	// data and dataDir are the store and the directory it's in, for a backup.
	data    *store.Store
	dataDir string
	secrets secrets
	// clients are the network API's. They're nil if its listener isn't on, or couldn't start.
	clients *clients
	// pin is the network API's public key, as clients check it, and address where it thinks it's
	// reached.
	pin, address string
	authFailures prometheus.Counter
}

// serve answers calls on the socket until the listener closes, and returns once every call has
// finished. The socket's permissions are its authentication.
func (s *apiServer) serve(l net.Listener) {
	routes := s.routes()
	s.serveWith(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routes.ServeHTTP(w, r.WithContext(withCaller(r.Context(), localCaller)))
	}))
}

// serveNetwork answers calls over TLS, each of which needs a client's token but a public one's.
func (s *apiServer) serveNetwork(l net.Listener) {
	s.serveWith(l, s.networkRoutes())
}

// networkRoutes is the API with a token checked on every route but a public one, which a stranger
// can call, so its body is kept small.
func (s *apiServer) networkRoutes() http.Handler {
	routes := s.routes()
	authenticated := s.authenticated(routes)
	public := map[string]bool{}
	for _, r := range api.Routes {
		if r.Public {
			public[r.Method+" "+r.Path] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !public[r.Method+" "+r.URL.Path] {
			authenticated.ServeHTTP(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxPublicRequest)
		routes.ServeHTTP(w, r.WithContext(withCaller(r.Context(), caller{name: "login"})))
	})
}

// authenticated refuses a call without a valid token before reading any of it.
func (s *apiServer) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		var name string
		var revoked <-chan struct{}
		known := false
		if ok {
			name, revoked, known = s.clients.check(token)
		}
		if !known {
			// Nothing of the body is read, and the connection goes, so a stranger can't keep it open.
			w.Header().Set("Connection", "close")
			_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(requestTimeout))
			s.authFailures.Inc()
			s.log.Info("Refused API request without a valid token", "remote", r.RemoteAddr, "path", r.URL.Path)
			w.Header().Set("WWW-Authenticate", `Bearer realm="oikade"`)
			respond(s.log, w, http.StatusUnauthorized, api.Response{
				Error: "the host needs a valid token for this: ask for one with oikade client add on the host's machine",
			})
			return
		}
		// A revoke ends the client's streams. A call that has begun still finishes, as it does without
		// its connection.
		ctx, cancel := context.WithCancel(withCaller(r.Context(), caller{name: name}))
		defer cancel()
		go func() {
			select {
			case <-revoked:
				cancel()
			case <-ctx.Done():
			}
		}()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *apiServer) serveWith(l net.Listener, handler http.Handler) {
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: requestTimeout,
		// oikade makes one call a connection, and another client may keep one open.
		IdleTimeout: idleTimeout,
		ErrorLog:    stdlog.New(httpErrors{s.log}, "", 0),
	}
	if err := server.Serve(l); !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
		s.log.Error("Failed to accept connection", "err", err)
	}
	if err := server.Shutdown(context.WithoutCancel(s.ctx)); err != nil {
		s.log.Error("Failed to finish API calls", "err", err)
	}
}

// routes is the API. Anything else gets a 404 that says which API this is, for a client from another
// version.
func (s *apiServer) routes() http.Handler {
	mux := http.NewServeMux()
	for _, r := range api.Routes {
		mux.Handle(r.Method+" "+r.Path, crashes(s.handler(r)))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		respond(s.log, w, http.StatusNotFound, api.Response{
			Error: fmt.Sprintf("no route %s %s: this host speaks %s", r.Method, r.URL.Path, api.Prefix),
		})
	})
	return mux
}

// crashes runs h on a goroutine of its own, so that a panic in a call crashes the host, as it does
// anywhere else, rather than net/http recovering it and leaving whatever it held half done.
func crashes(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			h(w, r)
		}()
		<-done
	}
}

func (s *apiServer) handler(route api.Route) http.HandlerFunc {
	switch route.Type {
	case api.TypeTail:
		return s.tail
	case api.TypeDeploy:
		return s.deployUpload
	case api.TypeEvents:
		return func(w http.ResponseWriter, r *http.Request) {
			s.log.Info("Received API request", "type", route.Type, "client", callerOf(r.Context()).name)
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			defer context.AfterFunc(s.ctx, cancel)()
			s.srv.events.stream(ctx, newLines(w), func(err error) {
				respond(s.log, w, http.StatusInternalServerError, api.Response{Error: err.Error()})
			})
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var req api.Request
		if err := s.read(w, r, &req); err != nil {
			s.log.Debug("Dropped API request", "err", err)
			respond(s.log, w, http.StatusBadRequest, api.Response{Error: fmt.Sprintf("reading the request: %v", err)})
			return
		}
		req.Type = route.Type
		resp := s.answer(callerOf(r.Context()), req)
		if route.Public && resp.Error != "" {
			// A stranger's call that failed, such as a wrong login code, keeps nothing open.
			w.Header().Set("Connection", "close")
		}
		respond(s.log, w, statusOf(resp), resp)
	}
}

// statusOf is a Response's status: a 422 for one that didn't do what was asked.
func statusOf(resp api.Response) int {
	if resp.Error != "" {
		return http.StatusUnprocessableEntity
	}
	return http.StatusOK
}

// read takes a POST's body within requestTimeout. An empty body is a request with no fields.
func (s *apiServer) read(w http.ResponseWriter, r *http.Request, req *api.Request) error {
	control := http.NewResponseController(w)
	_ = control.SetReadDeadline(time.Now().Add(requestTimeout))
	defer func() { _ = control.SetReadDeadline(time.Time{}) }()
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequest)).Decode(req)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// respond answers a call. The call's own work is done by now: a caller that went away changes nothing.
func respond(log *slog.Logger, w http.ResponseWriter, status int, resp api.Response) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(writeTimeout))
	w.Header().Set("Content-Type", api.ContentType)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Debug("Could not answer a request; the caller left", "err", err)
	}
}

// lines is a stream's answer, whose header goes with its first line.
type lines struct {
	w       http.ResponseWriter
	control *http.ResponseController
	out     *json.Encoder
	started bool
}

var _ lineWriter = (*lines)(nil)

func newLines(w http.ResponseWriter) *lines {
	return &lines{w: w, control: http.NewResponseController(w), out: json.NewEncoder(w)}
}

func (l *lines) write(v any) bool {
	if !l.started {
		l.w.Header().Set("Content-Type", api.StreamContentType)
		l.started = true
	}
	_ = l.control.SetWriteDeadline(time.Now().Add(writeTimeout))
	return l.out.Encode(v) == nil
}

func (l *lines) flush() bool {
	_ = l.control.SetWriteDeadline(time.Now().Add(writeTimeout))
	return l.control.Flush() == nil
}

// tail streams events until the caller goes, the host stops, or the caller falls behind.
func (s *apiServer) tail(w http.ResponseWriter, r *http.Request) {
	s.log.Info("Received API request", "type", api.TypeTail, "client", callerOf(r.Context()).name)
	events, stop := s.srv.feed.subscribe()
	defer stop()
	out := newLines(w)
	write := func(e api.Event) bool { return out.write(e) && out.flush() }
	if !write(api.Event{}) { // Says the feed has started.
		return
	}
	for {
		select {
		case e, ok := <-events:
			if !ok || !write(e) {
				return
			}
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *apiServer) answer(c caller, req api.Request) api.Response {
	s.log.Info("Received API request", "type", req.Type, "client", c.name)
	switch req.Type {
	case api.TypeClients, api.TypeClientAdd, api.TypeClientRevoke, api.TypeLogin:
		return s.client(c, req)
	case api.TypeInfo:
		return api.Response{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
	case api.TypeRollback:
		return s.deploys.rollback(s.ctx, req.AllowSafetyRemoval)
	case api.TypeStatus:
		return s.status()
	case api.TypeSet:
		return s.set(s.ctx, req)
	case api.TypePause:
		return s.pause(s.ctx, req)
	case api.TypeResume:
		return s.resume(s.ctx, req)
	case api.TypeMatter:
		return s.matter()
	case api.TypeBackup:
		return s.backup()
	case api.TypeSecrets, api.TypeSecretSet, api.TypeSecretRemove:
		return s.secret(req)
	case api.TypeDevices, api.TypeDeviceName, api.TypeDeviceRoom, api.TypeDeviceReserve,
		api.TypeDeviceRemove, api.TypeDeviceReplace, api.TypeDeviceRestore:
		return s.device(req)
	}
	return api.Response{Error: fmt.Sprintf("unknown request %q", req.Type)}
}

// client manages the network API's clients. Adding one works only on the socket, so a stolen token
// can't make more. Revoking works from anywhere, so a lost laptop can be cut off from another.
func (s *apiServer) client(c caller, req api.Request) api.Response {
	if s.clients == nil {
		return api.Response{Error: "the host has no network API clients: start it with --api-addr, and its log says why if it was"}
	}
	switch req.Type {
	case api.TypeClients:
		return api.Response{Clients: s.clients.list()}
	case api.TypeClientAdd:
		if !c.local {
			return api.Response{Error: errNotLocal.Error()}
		}
		if !req.Token && s.address == "" {
			return api.Response{Error: "the host isn't serving its network API, so nothing could log in: its log says why"}
		}
		if !req.Token {
			code, expires, err := s.clients.issue(req.Client)
			if err != nil {
				return api.Response{Error: err.Error()}
			}
			s.log.Info("Issued API login code", "name", req.Client, "expires", expires)
			return api.Response{Code: code, Expires: expires, Pin: s.pin, Address: s.address}
		}
		token, err := s.clients.add(req.Client)
		if err != nil {
			return api.Response{Error: err.Error()}
		}
		s.log.Info("Added API client", "name", req.Client)
		return api.Response{Token: token, Pin: s.pin}
	case api.TypeLogin:
		name, token, err := s.clients.redeem(req.Code)
		if err != nil {
			if !c.local {
				s.authFailures.Inc()
			}
			s.log.Info("Refused API login", "err", err)
			return api.Response{Error: err.Error()}
		}
		s.log.Info("Added API client", "name", name, "by", "login")
		return api.Response{Token: token, Client: name}
	case api.TypeClientRevoke:
		if err := s.clients.revoke(req.Client); err != nil {
			return api.Response{Error: err.Error()}
		}
		s.log.Info("Revoked API client", "name", req.Client, "client", c.name)
		return api.Response{}
	}
	return api.Response{Error: "unknown client request"}
}

// httpErrors takes net/http's own log lines, which aren't in this repo's style. A failed TLS handshake
// is anyone on the network, such as a port scan or a client that pins something else, so it's Debug.
type httpErrors struct{ log *slog.Logger }

func (h httpErrors) Write(p []byte) (int, error) {
	line := strings.TrimSpace(string(p))
	if strings.Contains(line, "TLS handshake error") {
		h.log.Debug("Failed TLS handshake", "err", line)
	} else {
		h.log.Warn("Logged HTTP server error", "err", line)
	}
	return len(p), nil
}
