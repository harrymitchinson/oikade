package store

import (
	"fmt"
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

// appendCost is the median of what a journal append allocates in the file, its pages and the
// freelist it rewrites, with a log commit before each one. It counts bytes rather than timing them,
// so a busy machine can't move it.
func appendCost(t *testing.T, s *Store, bound Bound, data []byte) int64 {
	t.Helper()
	batch := []Record{{Data: data}}
	costs := make([]int64, 0, 300)
	for i := range cap(costs) {
		batch[0].Time = time.Now()
		if err := s.Events().Add(batch, time.Now(), bound, countDropped); err != nil {
			t.Fatal(err)
		}
		before := s.db.Stats()
		if _, err := s.Journal().Append("run", data); err != nil {
			t.Fatal(err)
		}
		after := s.db.Stats()
		costs = append(costs, after.TxStats.GetPageAlloc()-before.TxStats.GetPageAlloc())
		if i%50 == 49 {
			if err := s.Journal().Delete("run"); err != nil {
				t.Fatal(err)
			}
		}
	}
	slices.Sort(costs)
	return costs[len(costs)/2]
}

func TestAFullLogDoesntSlowTheJournal(t *testing.T) {
	const full = 64 << 20
	data := make([]byte, 70)
	bound := Bound{MaxBytes: full}

	empty := open(t, t.TempDir())
	before := appendCost(t, empty, bound, data)

	filled := open(t, t.TempDir())
	batch := make([]Record, 20000)
	now := time.Now()
	for i := range batch {
		batch[i] = Record{Time: now, Data: data}
	}
	for {
		if err := filled.Events().Add(batch, now, bound, countDropped); err != nil {
			t.Fatal(err)
		}
		if size, err := filled.Events().Size(); err != nil {
			t.Fatal(err)
		} else if size >= full-20000*100 {
			break
		}
	}
	after := appendCost(t, filled, bound, data)
	t.Logf("median journal append writes %d bytes with an empty log, %d with a full one", before, after)
	if after > 2*before {
		t.Fatalf("median journal append went from %d to %d bytes with a full log", before, after)
	}
}
