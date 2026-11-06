package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/store"
)

// requestTimeout is how long a connection has to say what it wants.
const requestTimeout = 5 * time.Second

// writeTimeout bounds one event to a tail, so one that stopped reading can't hold it open.
const writeTimeout = 5 * time.Second

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

// serve accepts connections until the listener closes, and returns once every call has finished.
func (s *apiServer) serve(l net.Listener) {
	var calls sync.WaitGroup
	defer calls.Wait()
	for {
		c, err := l.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				s.log.Error("Failed to accept connection", "err", err)
			}
			return
		}
		calls.Go(func() { s.handle(c) })
	}
}

func (s *apiServer) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(requestTimeout))
	var req api.Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		s.log.Debug("Dropped connection before its request", "err", err)
		return
	}
	if req.Type == api.TypeTail {
		s.tail(conn)
		return
	}
	if req.Type == api.TypeEvents {
		s.srv.events.stream(s.ctx, conn)
		return
	}
	if err := json.NewEncoder(conn).Encode(s.answer(req)); err != nil {
		s.log.Debug("Could not answer a request; the caller left", "err", err)
	}
}

// tail streams events until the caller goes, the host stops, or the caller falls behind.
func (s *apiServer) tail(conn net.Conn) {
	events, stop := s.srv.feed.subscribe()
	defer stop()
	_ = conn.SetReadDeadline(time.Time{})
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		_, _ = io.Copy(io.Discard, conn)
	}()
	out := json.NewEncoder(conn)
	write := func(e api.Event) bool {
		_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		return out.Encode(e) == nil
	}
	if !write(api.Event{}) { // Says the feed has started.
		return
	}
	for {
		select {
		case e, ok := <-events:
			if !ok || !write(e) {
				return
			}
		case <-gone:
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
