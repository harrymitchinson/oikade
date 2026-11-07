package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"time"

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
}

// serve answers calls until the listener closes, and returns once every call has finished.
func (s *apiServer) serve(l net.Listener) {
	server := &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: requestTimeout,
		// oikade makes one call a connection, and another client may keep one open.
		IdleTimeout: idleTimeout,
		ErrorLog:    slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
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
	case api.TypeEvents:
		return func(w http.ResponseWriter, r *http.Request) {
			s.srv.events.stream(s.ctx, newLines(w), func(err error) {
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
		resp := s.answer(req)
		status := http.StatusOK
		if resp.Error != "" {
			status = http.StatusUnprocessableEntity
		}
		respond(s.log, w, status, resp)
	}
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

func (s *apiServer) answer(req api.Request) api.Response {
	s.log.Info("Received API request", "type", req.Type)
	switch req.Type {
	case api.TypeInfo:
		return api.Response{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
	case api.TypeDeploy:
		return s.deploys.deploy(s.ctx, req.Binary, req.AllowSafetyRemoval)
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
