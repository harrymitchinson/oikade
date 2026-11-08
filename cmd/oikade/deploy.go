package main

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// progress says on stderr what a deploy is doing, a line as each phase starts, so that its silent
// parts don't look hung. On a terminal, live, it also redraws the upload's bytes sent; anywhere
// else it writes whole lines only, with no carriage returns.
type progress struct {
	w    io.Writer
	live bool

	mu sync.Mutex
	// open is a line on screen still waiting for its newline.
	open bool
	// drawn is when the upload's line was last redrawn.
	drawn time.Time
	// uploaded is an upload that's finished.
	uploaded bool
}

// redraw is how often the upload's line changes at most.
const redraw = 100 * time.Millisecond

// phase starts a phase with its line.
func (p *progress) phase(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.endLine()
	_, _ = fmt.Fprintf(p.w, format+"\n", args...)
}

// startUpload is the upload's phase, of total bytes.
func (p *progress) startUpload(total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.endLine()
	p.uploaded = false
	if p.live {
		_, _ = fmt.Fprintf(p.w, "Uploading %s", size(total))
		p.open = true
		return
	}
	_, _ = fmt.Fprintf(p.w, "Uploading %s\n", size(total))
}

// upload is the upload's bytes sent so far, as api.Deploy reports them, from the goroutine sending
// them. Once they're all sent, the host is what's left to wait for.
func (p *progress) upload(sent, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.uploaded {
		return
	}
	done := sent >= total
	if p.live && p.open && (done || time.Since(p.drawn) >= redraw) {
		// \x1b[K clears what's left of a longer line before it.
		_, _ = fmt.Fprintf(p.w, "\rUploading %s: %s sent\x1b[K", size(total), size(sent))
		p.drawn = time.Now()
	}
	if done {
		p.uploaded = true
		p.endLine()
		_, _ = fmt.Fprintln(p.w, "Waiting for the host to check and settle")
	}
}

// end finishes a line left open by an upload that stopped part way.
func (p *progress) end() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.endLine()
}

func (p *progress) endLine() {
	if p.open {
		_, _ = fmt.Fprintln(p.w)
		p.open = false
	}
}

// heartbeat writes a line every interval until stop, to show something slow and quiet, such as a
// first build downloading modules, is still going.
func heartbeat(p *progress, what string, interval time.Duration) (stop func()) {
	start := time.Now()
	ticker := time.NewTicker(interval)
	quit, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		for {
			select {
			case <-quit:
				return
			case now := <-ticker.C:
				p.phase("Still %s, %s so far", what, now.Sub(start).Round(time.Second))
			}
		}
	}()
	return func() {
		ticker.Stop()
		close(quit)
		<-exited
	}
}

// size is a number of bytes for people, in decimal units: 11 MB, 840 kB.
func size(n int64) string {
	units := []string{"B", "kB", "MB", "GB"}
	v, i := float64(n), 0
	for v >= 1000 && i < len(units)-1 {
		v /= 1000
		i++
	}
	if i == 0 || v >= 10 {
		return fmt.Sprintf("%.0f %s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}
