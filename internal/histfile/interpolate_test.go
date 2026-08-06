package histfile

import (
	"testing"
	"time"
)

// A history file merged from two shells can have its dated entries out of
// order. Undated entries between them still have to come out in the order they
// were written: identical timestamps collapse distinct commands into one
// imported entry, because an import's identity is derived from its time.
func TestUndatedEntriesKeepTheirOrderAcrossADisorderedFile(t *testing.T) {
	for _, tc := range []struct {
		name       string
		prev, next int64
	}{
		{"in order", 1_700_000_000_000, 1_700_000_900_000},
		{"the same instant", 1_700_000_000_000, 1_700_000_000_000},
		{"backwards", 1_700_000_900_000, 1_700_000_000_000},
		// In order, but too close together for the gap to divide into three.
		{"a millisecond apart", 1_700_000_000_000, 1_700_000_000_001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := []Entry{
				{Command: "dated first", StartTime: tc.prev},
				{Command: "undated a"},
				{Command: "undated b"},
				{Command: "undated c"},
				{Command: "dated last", StartTime: tc.next},
			}
			approximateUndated(entries, time.UnixMilli(1_700_001_000_000))

			for i := 1; i <= 3; i++ {
				if entries[i].StartTime == 0 {
					t.Fatalf("entry %d got no time at all", i)
				}
				if !entries[i].Approximate {
					t.Errorf("entry %d was not flagged approximate", i)
				}
			}
			// Strictly increasing, which is what keeps them distinct and in
			// the order the user typed them.
			for i := 2; i <= 3; i++ {
				if entries[i].StartTime <= entries[i-1].StartTime {
					t.Errorf("entry %d (%d) does not come after entry %d (%d)",
						i, entries[i].StartTime, i-1, entries[i-1].StartTime)
				}
			}
		})
	}
}
