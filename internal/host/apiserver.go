package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/harrymitchinson/oikade/internal/host/api"
)

// requestTimeout is how long a connection has to say what it wants.
const requestTimeout = 5 * time.Second

// apiServer answers the oikade command. A call outlives its connection: a deploy that has begun
// finishes even if the command that asked goes away.
type apiServer struct {
	log     logr.Logger
	ctx     context.Context
	deploys *deployer
}

// serve accepts connections until the listener closes, and returns once every call has finished.
func (s *apiServer) serve(l net.Listener) {
	var calls sync.WaitGroup
	defer calls.Wait()
	for {
		c, err := l.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				s.log.Error(err, "Failed to accept connection")
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
		s.log.V(1).Info("Dropped connection before its request", "error", err.Error())
		return
	}
	if err := json.NewEncoder(conn).Encode(s.answer(req)); err != nil {
		s.log.V(1).Info("Could not answer a request; the caller left", "error", err.Error())
	}
}

func (s *apiServer) answer(req api.Request) api.Response {
	switch req.Type {
	case api.TypeInfo:
		return api.Response{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
	case api.TypeDeploy:
		return s.deploys.deploy(s.ctx, req.Binary, req.AllowSafetyRemoval)
	case api.TypeRollback:
		return s.deploys.rollback(s.ctx, req.AllowSafetyRemoval)
	}
	return api.Response{Error: fmt.Sprintf("unknown request %q", req.Type)}
}
