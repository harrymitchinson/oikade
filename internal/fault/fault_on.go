//go:build faultpoints

package fault

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// FileEnv names a file of faults, one per line: "<point> <n> <kill|drop|wait>", which acts on the
// point's nth hit in this process. A kill removes the file first, so the next process runs clean. A
// wait holds the point until the file's name with ".go" on the end exists.
const FileEnv = "OIKADE_FAULTS"

type planned struct {
	at   int
	drop bool
	wait bool
}

var (
	mu    sync.Mutex
	read  bool
	plan  map[string]planned
	calls = map[string]int{}
)

func load() {
	if read {
		return
	}
	read = true
	plan = map[string]planned{}
	raw, err := os.ReadFile(os.Getenv(FileEnv))
	if err != nil {
		return
	}
	for line := range strings.Lines(string(raw)) {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		n, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		plan[f[0]] = planned{at: n, drop: f[2] == "drop", wait: f[2] == "wait"}
	}
}

func due(point string, drop bool) bool {
	mu.Lock()
	defer mu.Unlock()
	load()
	p, ok := plan[point]
	if !ok || p.drop != drop {
		return false
	}
	calls[point]++
	return calls[point] == p.at
}

// Hit kills this process, as SIGKILL does, on the point's planned hit.
func Hit(point string) {
	if due(point, false) {
		mu.Lock()
		wait := plan[point].wait
		mu.Unlock()
		if wait {
			for {
				if _, err := os.Stat(os.Getenv(FileEnv) + ".go"); err == nil {
					return
				}
				time.Sleep(time.Millisecond)
			}
		}
		_ = os.Remove(os.Getenv(FileEnv))
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	}
}

// Drop reports whether this is the point's planned hit.
func Drop(point string) bool { return due(point, true) }
