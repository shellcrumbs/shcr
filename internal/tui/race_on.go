//go:build race

package tui

// raceEnabled reports whether the binary was built with the race detector,
// which slows execution by an order of magnitude or more. Timing budgets are
// meaningless under it.
const raceEnabled = true
