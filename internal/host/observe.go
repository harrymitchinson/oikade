package host

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/metrics"
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

// Gather implements prometheus.Gatherer.
func (p *pushed) Gather() ([]*dto.MetricFamily, error) {
	p.mu.Lock()
	text := p.text
	p.mu.Unlock()
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(bytes.NewReader(text))
	if err != nil {
		return nil, err
	}
	out := make([]*dto.MetricFamily, 0, len(families))
	for _, f := range families {
		out = append(out, f)
	}
	return out, nil
}

// gatherer is everything the host exports.
func gatherer(store *hub.Store, eng *engine.Engine, p *pushed) prometheus.Gatherer {
	registry := prometheus.NewRegistry()
	registry.MustRegister(metrics.Collector{Store: store}, eng.ControlLevel)
	return prometheus.Gatherers{registry, p}
}

// observe opens the HTTP listener for metrics and health, which reads and changes nothing, and is
// apart from the API for that reason. It returns what serves it until a context ends.
func (h *Host) observe(srv *server, eng *engine.Engine) func(context.Context) {
	if h.MetricsAddr == "" {
		return func(context.Context) {}
	}
	l, err := net.Listen("tcp", h.MetricsAddr)
	if err != nil {
		// Worth saying loudly, and not worth stopping a home for.
		h.Log.Error(err, "Failed to open the metrics listener", "address", h.MetricsAddr)
		return func(context.Context) {}
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(gatherer(srv.devices, eng, srv.pushed),
		promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError}))
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
			h.Log.Error(err, "Failed to serve metrics")
		}
		<-stopped
	}
}
