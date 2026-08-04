package store

import "testing"

func TestRankingMetricsIgnoreEmptyQueries(t *testing.T) {
	s := newTestStore(t)
	at := int64(1_800_000_000_000)

	// Opening the picker and taking the top row says nothing about the
	// ordering: the list was in time order and nothing was searched for.
	for i := range 20 {
		if err := s.RecordAcceptance(Acceptance{
			At: at + int64(i), Query: "", Chosen: "ls", Rank: 0, Results: 50,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if m, err := s.RankingMetrics(); err != nil || m.Searches != 0 {
		t.Fatalf("empty queries counted: %+v (err %v)", m, err)
	}

	// Four real searches: first, first, second, fifth.
	for i, r := range []int{0, 0, 1, 4} {
		if err := s.RecordAcceptance(Acceptance{
			At: at + int64(100+i), Query: "npm", Chosen: "npm run build",
			Rank: r, Results: 30,
		}); err != nil {
			t.Fatal(err)
		}
	}
	m, err := s.RankingMetrics()
	if err != nil {
		t.Fatal(err)
	}
	if m.Searches != 4 {
		t.Fatalf("searches = %d, want 4", m.Searches)
	}
	if m.TopOne != 0.5 {
		t.Errorf("top-1 = %.2f, want 0.50", m.TopOne)
	}
	if m.TopThree != 0.75 {
		t.Errorf("top-3 = %.2f, want 0.75", m.TopThree)
	}
	// (1 + 1 + 1/2 + 1/5) / 4
	if want := (1 + 1 + 0.5 + 0.2) / 4; m.MRR < want-0.001 || m.MRR > want+0.001 {
		t.Errorf("MRR = %.4f, want %.4f", m.MRR, want)
	}
	if m.MedianRank != 2 {
		t.Errorf("median rank = %d, want 2 (1-based)", m.MedianRank)
	}
}

// The log is local: it is not an event, so it does not sync, and not a command,
// so it does not export. And it can be dropped.
func TestAcceptancesCanBeForgotten(t *testing.T) {
	s := newTestStore(t)
	for i := range 5 {
		if err := s.RecordAcceptance(Acceptance{
			At: int64(i), Query: "secret-ish", Chosen: "x", Rank: 0, Results: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.ForgetAcceptances()
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("forgot %d, want 5", n)
	}
	if m, _ := s.RankingMetrics(); m.Searches != 0 {
		t.Errorf("metrics survive forgetting: %+v", m)
	}
	// Nothing in the event log mentions it, so nothing can sync it.
	var events int
	if err := s.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Errorf("recording a search wrote %d event(s); it must stay local", events)
	}
}
