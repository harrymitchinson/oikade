package host

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/VictoriaMetrics/metrics"

	"github.com/harrymitchinson/oikade/internal/hub"
	devicemetrics "github.com/harrymitchinson/oikade/internal/metrics"
)

const (
	metricsTimeout = 5 * time.Second
	// scrapeTimeout bounds waiting for a scrape when the host stops.
	scrapeTimeout = 2 * time.Second
)

// pushed is your process's latest rule metrics: rules run there, and Prometheus scrapes the host, so
// a deploy doesn't move its target.
type pushed struct {
	mu   sync.Mutex
	text []byte
}

func (p *pushed) set(text []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.text = text
}

// write writes the latest metrics as they came, ending their last line so nothing after joins it.
func (p *pushed) write(w io.Writer) {
	p.mu.Lock()
	text := p.text
	p.mu.Unlock()
	if len(text) == 0 {
		return
	}
	_, _ = w.Write(text)
	if !bytes.HasSuffix(text, []byte("\n")) {
		_, _ = w.Write([]byte("\n"))
	}
}

// export adds the devices' metrics and your process's to set, which has the rest of what the host
// exports.
func export(set *metrics.Set, store *hub.Store, p *pushed) {
	devicemetrics.Collector{Store: store}.Register(set)
	set.RegisterMetricsWriter(p.write)
}

// observe opens the HTTP listener for metrics and health, which reads and changes nothing, and is
// apart from the API for that reason. It returns what serves it until a context ends.
func (h *Host) observe(exported *metrics.Set, srv *server) func(context.Context) {
	if h.MetricsAddr == "" {
		return func(context.Context) {}
	}
	l, err := net.Listen("tcp", h.MetricsAddr)
	if err != nil {
		// Worth saying loudly, and not worth stopping a home for.
		h.Log.Error("Failed to open the metrics listener", "err", err, "address", h.MetricsAddr)
		return func(context.Context) {}
	}
	mux := http.NewServeMux()
	export(exported, srv.devices, srv.pushed)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		exported.WritePrometheus(w)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: metricsTimeout}
	h.Log.Info("Serving metrics and health", "address", l.Addr().String())
	return func(ctx context.Context) {
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			<-ctx.Done()
			last, cancel := context.WithTimeout(context.WithoutCancel(ctx), scrapeTimeout)
			defer cancel()
			_ = server.Shutdown(last)
		}()
		if err := server.Serve(l); !errors.Is(err, http.ErrServerClosed) {
			h.Log.Error("Failed to serve metrics", "err", err)
		}
		<-stopped
	}
}
