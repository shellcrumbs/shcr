package store

// Acceptance is one moment of a person taking a command out of the picker.
type Acceptance struct {
	At      int64
	Query   string
	Chosen  string
	Rank    int
	Results int
	Where   Where
}

// RecordAcceptance notes which candidate was taken.
//
// Local only: its own table, so it is neither an event that syncs nor a command
// that exports. What it is for is answering whether a change to the ranking
// helped, which nothing else in the system can say.
func (s *Store) RecordAcceptance(a Acceptance) error {
	_, err := s.db.Exec(`
		INSERT INTO picker_acceptances
		    (at, query, chosen, rank, results, cwd, hostname, session_id, branch)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		a.At, a.Query, a.Chosen, a.Rank, a.Results,
		a.Where.Cwd, a.Where.Hostname, a.Where.SessionID, a.Where.Branch)
	return err
}

// RankingMetrics summarises how well the ordering has been doing.
//
// Searches with nothing typed are excluded: the picker opens on recent history,
// so taking the top row says nothing about ranking and would flatter every
// number here.
type RankingMetrics struct {
	// Searches is how many acceptances followed a typed query.
	Searches int
	// TopOne is the share where the accepted command was already first.
	TopOne float64
	// TopThree is the share where it was in the first three.
	TopThree float64
	// MRR is the mean reciprocal rank: 1.0 if everything was first, 0.5 if
	// everything was second.
	MRR float64
	// MedianRank is 1-based, so 1 means "already at the top".
	MedianRank int
}

func (s *Store) RankingMetrics() (RankingMetrics, error) {
	var m RankingMetrics
	rows, err := s.db.Query(
		`SELECT rank FROM picker_acceptances WHERE query <> '' ORDER BY rank`)
	if err != nil {
		return m, err
	}
	defer rows.Close()

	var ranks []int
	for rows.Next() {
		var r int
		if err := rows.Scan(&r); err != nil {
			return m, err
		}
		ranks = append(ranks, r)
	}
	if err := rows.Err(); err != nil {
		return m, err
	}
	if len(ranks) == 0 {
		return m, nil
	}

	m.Searches = len(ranks)
	var top1, top3, mrr float64
	for _, r := range ranks {
		if r == 0 {
			top1++
		}
		if r < 3 {
			top3++
		}
		mrr += 1 / float64(r+1)
	}
	n := float64(len(ranks))
	m.TopOne, m.TopThree, m.MRR = top1/n, top3/n, mrr/n
	m.MedianRank = ranks[len(ranks)/2] + 1
	return m, nil
}

// ForgetAcceptances empties the log.
func (s *Store) ForgetAcceptances() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM picker_acceptances`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
