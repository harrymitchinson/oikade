package store

import (
	"fmt"
	"os"
	"slices"
	"testing"
	"time"
)

// countDropped is a Fold that keeps the number dropped as its baseline.
func countDropped(baseline []byte, dropped []Record) ([]byte, error) {
	n := 0
	if baseline != nil {
		if _, err := fmt.Sscan(string(baseline), &n); err != nil {
			return nil, err
		}
	}
	return fmt.Appendf(nil, "%d", n+len(dropped)), nil
}

func TestEventsComeBackInOrderAcrossPages(t *testing.T) {
	s := open(t, t.TempDir())
	at := time.Date(2030, 1, 7, 8, 0, 0, 0, time.UTC)
	for i := range 5 {
		r := Record{Time: at.Add(time.Duration(i) * time.Second), Data: fmt.Appendf(nil, "%d", i)}
		if err := s.Events().Add([]Record{r}, at, Bound{}, countDropped); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.Events().Read(0, 3)
	if err != nil {
		t.Fatal(err)
	}
	rest, err := s.Events().Read(first[len(first)-1].Seq, 3)
	if err != nil {
		t.Fatal(err)
	}
	all := slices.Concat(first, rest)
	if len(all) != 5 {
		t.Fatalf("read %d records, want 5", len(all))
	}
	for i, r := range all {
		if string(r.Data) != fmt.Sprint(i) || !r.Time.Equal(at.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("record %d is %s at %s", i, r.Data, r.Time)
		}
	}
}

func TestAnEmptyLogReadsAsEmpty(t *testing.T) {
	s := open(t, t.TempDir())
	records, err := s.Events().Read(0, 10)
	if err != nil || len(records) != 0 {
		t.Fatalf("Read = %v, %v", records, err)
	}
	if raw, first, err := s.Events().Start(10); err != nil || raw != nil || len(first) != 0 {
		t.Fatalf("Start = %q, %v, %v", raw, first, err)
	}
}

// A month of a busy home, added as the host adds it, against a bound smaller than the month.
func TestLogStaysInsideItsBoundOverAMonth(t *testing.T) {
	cases := map[string]Bound{
		"by size": {MaxBytes: 4096},
		"by age":  {MaxAge: 7 * 24 * time.Hour},
	}
	for name, bound := range cases {
		t.Run(name, func(t *testing.T) {
			s := open(t, t.TempDir())
			start := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
			data := make([]byte, 70)
			added := 0
			for hour := 0; hour < 30*24; hour += 6 {
				now := start.Add(time.Duration(hour) * time.Hour)
				batch := make([]Record, 60)
				for i := range batch {
					batch[i] = Record{Time: now.Add(time.Duration(i-len(batch)) * time.Minute), Data: data}
				}
				added += len(batch)
				if err := s.Events().Add(batch, now, bound, countDropped); err != nil {
					t.Fatal(err)
				}
				size, err := s.Events().Size()
				if err != nil {
					t.Fatal(err)
				}
				if bound.MaxBytes > 0 && size > bound.MaxBytes {
					t.Fatalf("hour %d: the log holds %d bytes, over %d", hour, size, bound.MaxBytes)
				}
			}
			left, err := s.Events().Read(0, added)
			if err != nil {
				t.Fatal(err)
			}
			if len(left) == 0 || len(left) == added {
				t.Fatalf("%d of %d records left", len(left), added)
			}
			if bound.MaxAge > 0 {
				end := start.Add((30*24 - 6) * time.Hour)
				if oldest := left[0].Time; oldest.Before(end.Add(-bound.MaxAge)) {
					t.Fatalf("the oldest record is from %s, past the bound from %s", oldest, end)
				}
			}
			if left[len(left)-1].Seq != uint64(added) {
				t.Fatalf("the newest record is %d, want %d: it isn't the oldest that went", left[len(left)-1].Seq, added)
			}
			baseline, _, err := s.Events().Start(1)
			if err != nil {
				t.Fatal(err)
			}
			if want := fmt.Sprint(added - len(left)); string(baseline) != want {
				t.Fatalf("the baseline took in %s records, want %s", baseline, want)
			}
		})
	}
}

func TestABoundAloneDropsOnAQuietLog(t *testing.T) {
	s := open(t, t.TempDir())
	at := time.Date(2030, 1, 7, 8, 0, 0, 0, time.UTC)
	if err := s.Events().Add([]Record{{Time: at, Data: []byte("old")}}, at, Bound{}, countDropped); err != nil {
		t.Fatal(err)
	}
	if err := s.Events().Add(nil, at.Add(2*time.Hour), Bound{MaxAge: time.Hour}, countDropped); err != nil {
		t.Fatal(err)
	}
	left, err := s.Events().Read(0, 10)
	if err != nil || len(left) != 0 {
		t.Fatalf("Read = %v, %v", left, err)
	}
	if size, err := s.Events().Size(); err != nil || size != 0 {
		t.Fatalf("Size = %d, %v", size, err)
	}
}

func TestSeqKeepsRisingPastDropsAndARestart(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	at := time.Date(2030, 1, 7, 8, 0, 0, 0, time.UTC)
	one := []Record{{Time: at, Data: []byte("x")}}
	for range 3 {
		if err := s.Events().Add(one, at, Bound{MaxBytes: 1}, countDropped); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir)
	if err := s.Events().Add(one, at, Bound{}, countDropped); err != nil {
		t.Fatal(err)
	}
	left, err := s.Events().Read(0, 10)
	if err != nil || len(left) != 1 || left[0].Seq != 4 {
		t.Fatalf("Read = %+v, %v", left, err)
	}
}

// commitCosts are the medians of what the log's commit and then a journal append allocate in the
// file: the pages each one dirties and the freelist it rewrites, as bbolt counts them. Bytes rather
// than time, so a busy machine can't move them. What they miss is work inside a commit that
// allocates nothing, such as a scan.
func commitCosts(t *testing.T, s *Store, bound Bound, data []byte) (logAdd, journalAppend int64) {
	t.Helper()
	allocated := func() int64 {
		stats := s.db.Stats()
		return stats.TxStats.GetPageAlloc()
	}
	batch := []Record{{Data: data}}
	adds, appends := make([]int64, 0, 300), make([]int64, 0, 300)
	for i := range cap(appends) {
		batch[0].Time = time.Now()
		began := allocated()
		if err := s.Events().Add(batch, time.Now(), bound, countDropped); err != nil {
			t.Fatal(err)
		}
		added := allocated()
		if _, err := s.Journal().Append("run", data); err != nil {
			t.Fatal(err)
		}
		adds, appends = append(adds, added-began), append(appends, allocated()-added)
		if i%50 == 49 {
			if err := s.Journal().Delete("run"); err != nil {
				t.Fatal(err)
			}
		}
	}
	slices.Sort(adds)
	slices.Sort(appends)
	return adds[len(adds)/2], appends[len(appends)/2]
}

// atBound opens a store whose log has reached bound, so each commit drops what's oldest.
func atBound(t *testing.T, bound Bound, data []byte) *Store {
	t.Helper()
	s := open(t, t.TempDir())
	batch := make([]Record, 20000)
	now := time.Now()
	for i := range batch {
		batch[i] = Record{Time: now, Data: data}
	}
	for dropped := false; !dropped; {
		err := s.Events().Add(batch, now, bound, func(baseline []byte, out []Record) ([]byte, error) {
			dropped = len(out) > 0
			return countDropped(baseline, out)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// The host commits the log every few seconds, and the journal on every step of a flow. Neither
// should cost more as the log grows: a commit that drops the oldest record costs the same in a log
// four times the size, give or take a page for a deeper tree, and a journal append the same as with
// no log at all.
func TestAFullLogDoesntSlowTheJournal(t *testing.T) {
	const full = 64 << 20
	data := make([]byte, 70)
	page := int64(os.Getpagesize())

	_, emptyAppend := commitCosts(t, open(t, t.TempDir()), Bound{MaxBytes: full}, data)
	smallAdd, _ := commitCosts(t, atBound(t, Bound{MaxBytes: full / 4}, data), Bound{MaxBytes: full / 4}, data)
	fullAdd, fullAppend := commitCosts(t, atBound(t, Bound{MaxBytes: full}, data), Bound{MaxBytes: full}, data)
	t.Logf("median allocated by a log commit: %d bytes at %d MiB, %d at %d MiB", smallAdd, full>>22, fullAdd, full>>20)
	t.Logf("median allocated by a journal append: %d bytes with no log, %d with a full one", emptyAppend, fullAppend)
	if fullAdd > smallAdd+page {
		t.Errorf("a log commit went from %d to %d bytes in a log four times the size", smallAdd, fullAdd)
	}
	if fullAppend > emptyAppend+page {
		t.Errorf("a journal append went from %d to %d bytes with a full log", emptyAppend, fullAppend)
	}
}
