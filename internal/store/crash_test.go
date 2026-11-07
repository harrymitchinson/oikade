package store

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// childEnv makes the test binary act as a child process, with the data directory as its value.
const (
	childAppendEnv = "OIKADE_STORE_CHILD_APPEND"
	childHoldEnv   = "OIKADE_STORE_CHILD_HOLD"
	childWriters   = 4
	pageHeaderSize = 16
	metaSize       = 64
)

func TestMain(m *testing.M) {
	if dir := os.Getenv(childAppendEnv); dir != "" {
		childAppend(dir)
		return
	}
	if dir := os.Getenv(childHoldEnv); dir != "" {
		childHold(dir)
		return
	}
	os.Exit(m.Run())
}

// entryData is what entry seq of a run holds. Sizes vary so commits touch different pages.
func entryData(run string, seq uint64) []byte {
	return bytes.Repeat(fmt.Appendf(nil, "%s/%d;", run, seq), 1+int(seq%97))
}

// childAppend appends forever from several goroutines, and prints each entry only once Append has
// returned. It runs until it's killed.
func childAppend(dir string) {
	s, err := Open(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	j := s.Journal()
	for w := range childWriters {
		go func() {
			run := "run-" + strconv.Itoa(w)
			for {
				entries, err := j.Entries(run)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(1)
				}
				// bbolt allocates seq, so this is the one Append will return.
				next := uint64(1)
				if len(entries) > 0 {
					next = entries[len(entries)-1].Seq + 1
				}
				seq, err := j.Append(run, entryData(run, next))
				if err != nil || seq != next {
					fmt.Fprintln(os.Stderr, seq, next, err)
					os.Exit(1)
				}
				// One short write, so lines from different goroutines never mix.
				_, _ = fmt.Fprintf(os.Stdout, "%s %d\n", run, seq)
			}
		}()
	}
	select {}
}

// childHold opens the store and says so, then waits to be killed.
func childHold(dir string) {
	s, err := Open(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("open")
	// Not select{}: with nothing else running, the runtime exits on a deadlock.
	time.Sleep(time.Hour)
	runtime.KeepAlive(s)
}

func child(t *testing.T, env, dir string) (*exec.Cmd, io.Reader) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), env+"="+dir)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd, stdout
}

// check fails the test if the store's pages don't add up.
func check(t *testing.T, s *Store) {
	t.Helper()
	if err := s.db.View(func(tx *bolt.Tx) error {
		for err := range tx.Check() {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAcknowledgedEntriesSurviveSIGKILL(t *testing.T) {
	const kills = 40
	dir := t.TempDir()
	acked := map[string]uint64{}
	for kill := range kills {
		cmd, stdout := child(t, childAppendEnv, dir)
		var mu sync.Mutex
		var lines sync.WaitGroup
		lines.Go(func() {
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				run, seq, ok := strings.Cut(scanner.Text(), " ")
				n, err := strconv.ParseUint(seq, 10, 64)
				if !ok || err != nil {
					// The kill can only cut the last line, which nobody was told about.
					return
				}
				mu.Lock()
				acked[run] = max(acked[run], n)
				mu.Unlock()
			}
		})
		time.Sleep(time.Duration(20+rand.IntN(100)) * time.Millisecond)
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		lines.Wait()
		_ = cmd.Wait()

		s, err := Open(dir)
		if err != nil {
			t.Fatalf("kill %d: %v", kill, err)
		}
		check(t, s)
		for run, last := range acked {
			entries, err := s.Journal().Entries(run)
			if err != nil {
				t.Fatal(err)
			}
			if uint64(len(entries)) < last {
				t.Fatalf("kill %d: %s has %d entries, %d were acknowledged", kill, run, len(entries), last)
			}
			for i, e := range entries {
				if e.Seq != uint64(i+1) || !bytes.Equal(e.Data, entryData(run, e.Seq)) {
					t.Fatalf("kill %d: %s entry %d is wrong: seq %d", kill, run, i, e.Seq)
				}
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	var total uint64
	for _, n := range acked {
		total += n
	}
	if total == 0 {
		t.Fatal("the child never acknowledged an entry, so this proved nothing")
	}
	t.Logf("%d kills, %d acknowledged entries", kills, total)
}

func TestSecondProcessCannotOpen(t *testing.T) {
	dir := t.TempDir()
	cmd, stdout := child(t, childHoldEnv, dir)
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "open\n" {
		t.Fatalf("child said %q, %v", line, err)
	}
	s, err := Open(dir)
	if !errors.Is(err, ErrLocked) {
		if s != nil {
			_ = s.Close()
		}
		t.Fatalf("Open = %v, want ErrLocked", err)
	}
	if !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), "in use by another process") {
		t.Errorf("error doesn't say what's wrong: %v", err)
	}
	// A crash releases the lock.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	s = open(t, dir)
	check(t, s)
}

// commit returns the store's file after three acknowledged entries, and after one more.
func commit(t *testing.T, dir string) (before, after []byte, pageSize int) {
	t.Helper()
	s := open(t, dir)
	j := s.Journal()
	for seq := range uint64(3) {
		if _, err := j.Append("run", entryData("run", seq+1)); err != nil {
			t.Fatal(err)
		}
	}
	pageSize = s.db.Info().PageSize
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s = open(t, dir)
	if _, err := s.Journal().Append("run", entryData("run", 4)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	after, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return before, after, pageSize
}

// changedPages returns which pages differ, the meta page apart from the rest. bbolt writes and
// syncs data pages, in no fixed order, and only then writes the meta page that makes them live.
func changedPages(t *testing.T, before, after []byte, pageSize int) (meta int, data []int) {
	t.Helper()
	meta = -1
	for p := 0; p*pageSize < len(after); p++ {
		start, end := p*pageSize, (p+1)*pageSize
		if end <= len(before) && bytes.Equal(before[start:end], after[start:end]) {
			continue
		}
		if p < 2 {
			if meta != -1 {
				t.Fatal("both meta pages changed in one commit")
			}
			meta = p
			continue
		}
		data = append(data, p)
	}
	if meta == -1 || len(data) == 0 {
		t.Fatalf("meta page %d, data pages %v: this commit shows nothing", meta, data)
	}
	return meta, data
}

// expectFirstThree opens a crash image and checks it has exactly the acknowledged entries, and can
// carry on.
func expectFirstThree(t *testing.T, image []byte) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileName), image, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	check(t, s)
	entries, err := s.Journal().Entries("run")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want the 3 acknowledged", len(entries))
	}
	for _, e := range entries {
		if !bytes.Equal(e.Data, entryData("run", e.Seq)) {
			t.Fatalf("entry %d is wrong", e.Seq)
		}
	}
	seq, err := s.Journal().Append("run", entryData("run", 4))
	if err != nil || seq != 4 {
		t.Fatalf("Append after recovery = %d, %v", seq, err)
	}
	check(t, s)
}

func TestCrashBeforeTheMetaPageLosesOnlyTheUnacknowledgedEntry(t *testing.T) {
	before, after, pageSize := commit(t, t.TempDir())
	_, data := changedPages(t, before, after, pageSize)
	for range 50 {
		// Any subset of the data pages reached the disk, and the meta page didn't.
		image := make([]byte, max(len(before), len(after)))
		copy(image, before)
		for _, p := range data {
			switch rand.IntN(3) {
			case 0: // Reached the disk.
				copy(image[p*pageSize:(p+1)*pageSize], after[p*pageSize:])
			case 1: // Torn at a sector.
				copy(image[p*pageSize:p*pageSize+512*(1+rand.IntN(pageSize/512-1))], after[p*pageSize:])
			}
		}
		expectFirstThree(t, image)
	}
}

func TestTornMetaPageLosesOnlyTheUnacknowledgedEntry(t *testing.T) {
	before, after, pageSize := commit(t, t.TempDir())
	meta, _ := changedPages(t, before, after, pageSize)
	// Every data page landed, and the meta page stops at each byte of its checksummed fields.
	for cut := pageHeaderSize + 1; cut < pageHeaderSize+metaSize; cut++ {
		image := bytes.Clone(after)
		copy(image[meta*pageSize:], before[meta*pageSize:(meta+1)*pageSize])
		copy(image[meta*pageSize:meta*pageSize+cut], after[meta*pageSize:])
		expectFirstThree(t, image)
	}
}

func TestNewestMetaPageIsTheOneThatChanged(t *testing.T) {
	// A guard on the two tests above: if bbolt's meta layout moved, they'd tear the wrong bytes.
	before, after, pageSize := commit(t, t.TempDir())
	meta, _ := changedPages(t, before, after, pageSize)
	txid := func(file []byte, page int) uint64 {
		// Page header, then magic, version, page size, flags, root, freelist and pgid.
		return binary.LittleEndian.Uint64(file[page*pageSize+pageHeaderSize+48:])
	}
	if txid(after, meta) <= txid(after, 1-meta) || txid(after, meta) != txid(before, 1-meta)+1 {
		t.Fatalf("meta %d: txids before %d, %d, after %d, %d", meta,
			txid(before, 0), txid(before, 1), txid(after, 0), txid(after, 1))
	}
}
