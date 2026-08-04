package tui

import (
	"sort"

	"github.com/shellcrumbs/shcr/internal/rank"
	"github.com/shellcrumbs/shcr/internal/store"
)

// refineCandidates is how many commands survive the first pass and get a
// database lookup. Nobody perceives the ordering of result two hundred, and
// this is the only part of a keystroke that touches disk.
const refineCandidates = 200

// rankedResults answers one query.
//
// Two phases, because the signals have very different costs. The first scores
// every cached command on match quality and frecency, which needs nothing but
// arithmetic. Only the survivors get the second, which asks the database where
// each command has actually been run.
func rankedResults(st *store.Store, stats []store.CommandStat, where store.Where,
	query, status string, now int64) ([]store.Command, error) {

	tokens := rank.Tokens(query)

	// With no query there is nothing to rank: the picker opens on the history,
	// and the history is in the order it happened. Ordering an empty query by
	// use would put this morning's habit above the command just run, which is
	// never what the person who just pressed Ctrl+R meant.
	if len(tokens) == 0 {
		names := make([]string, 0, refineCandidates)
		for _, s := range stats {
			if !statusPossible(s, status) {
				continue
			}
			names = append(names, s.Command)
			if len(names) == refineCandidates {
				break
			}
		}
		rows, err := st.LatestExecutions(names)
		if err != nil {
			return nil, err
		}
		out := make([]store.Command, 0, len(names))
		for _, n := range names {
			if c, ok := rows[n]; ok && statusMatches(c, status) {
				out = append(out, c)
			}
		}
		return out, nil
	}

	type scored struct {
		stat  store.CommandStat
		match rank.Match
		score float64
	}

	// Phase one: match and frecency only.
	shortlist := make([]scored, 0, refineCandidates)
	for _, s := range stats {
		if !statusPossible(s, status) {
			continue
		}
		m, ok := rank.MatchCommand(s.Command, tokens)
		if !ok {
			continue
		}
		shortlist = append(shortlist, scored{
			stat:  s,
			match: m,
			score: rank.Score(m, s.Frecency.Value(now), rank.Context{}),
		})
	}
	sortScored := func(list []scored) {
		sort.SliceStable(list, func(i, j int) bool {
			return rank.Less(list[i].match.Tier, list[i].score, list[i].stat.LastTime,
				list[j].match.Tier, list[j].score, list[j].stat.LastTime)
		})
	}
	sortScored(shortlist)
	if len(shortlist) > refineCandidates {
		shortlist = shortlist[:refineCandidates]
	}
	if len(shortlist) == 0 {
		return nil, nil
	}

	// Phase two: where has each of these actually been run.
	names := make([]string, len(shortlist))
	for i, s := range shortlist {
		names[i] = s.stat.Command
	}
	contexts, err := st.CommandContexts(names, where)
	if err != nil {
		return nil, err
	}
	rows, err := st.LatestExecutions(names)
	if err != nil {
		return nil, err
	}
	for i := range shortlist {
		ctx := contextFor(shortlist[i].stat, contexts[shortlist[i].stat.Command], now)
		shortlist[i].score = rank.Score(shortlist[i].match,
			shortlist[i].stat.Frecency.Value(now), ctx)
	}
	sortScored(shortlist)

	out := make([]store.Command, 0, len(shortlist))
	for _, s := range shortlist {
		if c, ok := rows[s.stat.Command]; ok && statusMatches(c, status) {
			out = append(out, c)
		}
	}
	return out, nil
}

// contextFor turns what the cache knows about a command, plus where it has been
// run, into the multipliers the scorer wants.
func contextFor(s store.CommandStat, where store.CommandContext, now int64) rank.Context {
	ctx := rank.Context{
		SameDir:     where.SameDir,
		SameRepo:    where.SameRepo,
		SameSession: where.SameSession,
		SameHost:    where.SameHost,
		SameBranch:  where.SameBranch,
		AllImported: s.Runs > 0 && s.ImportedRuns == s.Runs,
	}
	// Interrupted executions are excluded from the denominator as well as the
	// numerator: Ctrl-C is neither a success nor a failure, and counting it as
	// either would make every long-running command look bad.
	finished := s.Succeeded + s.Failed + s.NeverRan
	if finished > 0 {
		ctx.FailureRate = float64(s.Failed) / float64(finished)
	}
	// Only when it has *never* worked. A command that used to be missing and
	// now runs is just a command.
	ctx.NeverRan = s.NeverRan > 0 && s.Succeeded == 0 && s.Failed == 0
	ctx.Debugging = s.LastFailedAt > 0 && now-s.LastFailedAt <= rank.DebugWindow.Milliseconds()
	return ctx
}

// statusPossible is a cheap gate on the cached counts, so a status filter does
// not have to fetch a row to rule a command out. It admits more than it should
// — the counts say a command has failed at some point, not that it failed last
// time — and statusMatches settles it on the row itself.
func statusPossible(s store.CommandStat, status string) bool {
	switch status {
	case "":
		return true
	case store.StatusRunning, store.StatusOrphaned:
		return s.Unfinished > 0
	case store.StatusFailed:
		return s.Failed > 0 || s.NeverRan > 0
	case store.StatusCompleted:
		return s.Succeeded > 0 || s.ImportedRuns > 0
	}
	return true
}

func statusMatches(c store.Command, status string) bool {
	return status == "" || c.Status == status
}
