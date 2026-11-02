package transport

import "testing"

func TestReceptionEncrypted(t *testing.T) {
	var r reception
	steps := []struct {
		c    uint32
		want counterVerdict
	}{
		{100, counterNew}, // the first is trusted
		{100, counterDuplicate},
		{101, counterNew},
		{99, counterNew}, // late but inside the window
		{99, counterDuplicate},
		{101, counterDuplicate},
		{140, counterNew}, // a jump past the window clears it
		{108, counterNew}, // 140-32: still inside
		{108, counterDuplicate},
		{107, counterTooOld}, // 140-33
		{101, counterTooOld},
		{141, counterNew},
		{140, counterDuplicate},
	}
	for i, s := range steps {
		if got := r.check(s.c); got != s.want {
			t.Fatalf("step %d: check(%d) = %d, want %d", i, s.c, got, s.want)
		}
		if s.want == counterNew {
			r.accept(s.c)
		}
	}
}

func TestReceptionShiftKeepsHistory(t *testing.T) {
	var r reception
	for _, c := range []uint32{10, 12, 15} {
		r.accept(c)
	}
	for c, want := range map[uint32]counterVerdict{10: counterDuplicate, 11: counterNew, 12: counterDuplicate, 13: counterNew, 14: counterNew, 15: counterDuplicate} {
		if got := r.check(c); got != want {
			t.Errorf("check(%d) = %d, want %d", c, got, want)
		}
	}
	r.accept(15 + windowSize) // 15 is now exactly at the window's edge
	if got := r.check(15); got != counterDuplicate {
		t.Errorf("check(15) after a full shift = %d, want duplicate", got)
	}
	if got := r.check(14); got != counterTooOld {
		t.Errorf("check(14) after a full shift = %d, want too old", got)
	}
}

func TestReceptionUnencrypted(t *testing.T) {
	r := reception{rollover: true}
	steps := []struct {
		c    uint32
		want counterVerdict
	}{
		{0xFFFFFFFE, counterNew},
		{0xFFFFFFFF, counterNew},
		{0, counterNew}, // rolls over
		{0xFFFFFFFF, counterDuplicate},
		{1, counterNew},
		{0xFFFFFFFD, counterNew}, // 4 behind, never seen
		{0xFFFFFFFD, counterDuplicate},
		{500, counterNew}, // a jump restarts the window
		{1, counterNew},   // far behind: a peer that restarted its counter
		{1, counterDuplicate},
		{0, counterNew}, // 1 behind the restarted window
	}
	for i, s := range steps {
		if got := r.check(s.c); got != s.want {
			t.Fatalf("step %d: check(%#x) = %d, want %d", i, s.c, got, s.want)
		}
		if s.want == counterNew {
			r.accept(s.c)
		}
	}
}
