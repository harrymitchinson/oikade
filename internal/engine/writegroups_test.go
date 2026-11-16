package engine

import "testing"

// A group's write over two rooms joins their writes into one ordered group, and leaves another
// room's to run on its own.
func TestAGroupsWriteJoinsOnlyTheRoomsItReaches(t *testing.T) {
	got := joinGroups([][]string{
		{"room/hall"},
		{"room/garage"},
		{"room/study", "room/hall"},
		{"room/study"},
		{"device/porch-light"},
	})
	if got[0] != got[2] || got[2] != got[3] {
		t.Errorf("the hall's, the group's and the study's writes are in %v, want one group", got)
	}
	if got[1] == got[0] || got[4] == got[0] || got[1] == got[4] {
		t.Errorf("the garage's and the porch light's writes joined others: %v", got)
	}
}
