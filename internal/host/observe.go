package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
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
	// refused is set while the last push was refused.
	refused bool
}

// pushedPrefix starts every family your process may push: its rules' metrics, and none the host
// exports itself.
const pushedPrefix = "oikade_rule_"

// set keeps text as your process's latest metrics if it's only its rules' metrics, and otherwise
// keeps none, so a bad push can't spoil the host's scrape. It says why only for the first push it
// refuses after one it kept, so a process that keeps sending them is heard once.
func (p *pushed) set(text []byte) error {
	err := checkPushed(text)
	p.mu.Lock()
	defer p.mu.Unlock()
	first := !p.refused
	p.text, p.refused = text, err != nil
	if err == nil {
		return nil
	}
	p.text = nil
	if !first {
		return nil
	}
	return err
}

// checkPushed returns why text isn't your process's rule metrics, line by line, or nil.
func checkPushed(text []byte) error {
	for line := range strings.Lines(string(text)) {
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			continue
		}
		if comment, ok := strings.CutPrefix(line, "# "); ok {
			fields := strings.Fields(comment)
			switch {
			case len(fields) >= 2 && fields[0] == "HELP" && strings.HasPrefix(fields[1], pushedPrefix):
			case len(fields) == 3 && fields[0] == "TYPE" && strings.HasPrefix(fields[1], pushedPrefix) &&
				slices.Contains([]string{"counter", "gauge", "histogram", "summary", "untyped"}, fields[2]):
			default:
				return fmt.Errorf("%q isn't a rule metric's HELP or TYPE", line)
			}
			continue
		}
		space := strings.LastIndexByte(line, ' ')
		if space < 0 || !strings.HasPrefix(line, pushedPrefix) {
			return fmt.Errorf("%q isn't a rule metric's sample", line)
		}
		if err := metrics.ValidateMetric(line[:space]); err != nil {
			return fmt.Errorf("series %q: %w", line[:space], err)
		}
		if _, err := strconv.ParseFloat(line[space+1:], 64); err != nil {
			return fmt.Errorf("series %q has value %q", line[:space], line[space+1:])
		}
	}
	return nil
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
