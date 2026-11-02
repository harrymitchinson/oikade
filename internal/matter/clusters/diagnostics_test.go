package clusters

import (
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/matter/im"
	"github.com/harrymitchinson/oikade/internal/matter/tlv"
)

func TestTimeSnapshot(t *testing.T) {
	f := newFixture(t)
	res := invoke(t, f.client, IDGeneralDiagnostics, 0x01, nil, false)
	fields := response(t, res, 0x02)
	if _, ok := fields[0].(uint64); !ok {
		t.Errorf("SystemTimeMs = %v", fields[0])
	}
	if posix, _ := fields[1].(uint64); time.Since(time.UnixMilli(int64(posix))).Abs() > time.Minute {
		t.Errorf("PosixTimeMs = %v", fields[1])
	}
	if v := value(t, readAttr(t, f.client, IDGeneralDiagnostics, 0x08)); v != false {
		t.Errorf("TestEventTriggersEnabled = %v", v)
	}
}

func TestIdentifyCountsDown(t *testing.T) {
	f := newFixture(t)
	path := im.AttributePath{Endpoint: 2, Cluster: IDIdentify, Attribute: 0}
	identifyTime := func() any {
		t.Helper()
		reports, err := f.client.Read(timeout(t), path)
		if err != nil || len(reports) != 1 {
			t.Fatalf("read IdentifyTime: %+v, %v", reports, err)
		}
		return value(t, reports[0])
	}
	res, err := f.client.Invoke(timeout(t), im.CommandPath{Endpoint: 2, Cluster: IDIdentify, Command: 0},
		func(w *tlv.Writer) { w.PutUint(tlv.Context(0), 1) }, false, 0)
	if err != nil || res.Status != im.StatusSuccess {
		t.Fatalf("Identify: %+v, %v", res, err)
	}
	if v := identifyTime(); v != uint64(1) {
		t.Errorf("IdentifyTime = %v, want 1", v)
	}
	waitFor(t, "IdentifyTime to run out", func() bool { return identifyTime() == uint64(0) })
	if f.changes.Load() != 0 {
		t.Errorf("Identify changed the switch")
	}
}
