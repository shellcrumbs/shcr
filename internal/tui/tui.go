// Package tui is the Ctrl+R picker.
//
// Two rules shape everything here. The picker never executes anything — the
// chosen command goes to stdout and the shell puts it in the prompt, editable.
// And it must feel instant: the query runs off the update loop so typing is
// never blocked by the database, and results that arrive late for a query the
// user has already moved past are discarded rather than rendered.
package tui

import (
	"encoding/base64"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/shellcrumbs/shcr/internal/ipc"
	"github.com/shellcrumbs/shcr/internal/store"
	"github.com/shellcrumbs/shcr/internal/theme"
)

// Below this many columns the detail pane is dropped entirely rather than
// squeezed — a cramped two-pane layout reads worse than a clean list.
const minSplitWidth = 100

// maxDetailWidth is all the detail pane can use. Its longest real line is a
// label and a hostname; the rest of the terminal belongs to the command text.
const maxDetailWidth = 36

const (
	// Five each side of the selected command. Enough to recognise what you were
	// doing at the time, without the pane becoming a second history list.
	sessionContextLines = 5
	queryLimit          = 200
)

var statusCycle = []string{"", store.StatusRunning, store.StatusFailed, store.StatusOrphaned}

type Model struct {
	store *store.Store
	theme *theme.Theme
	// localHost suppresses the host chip for commands that ran on this machine.
	localHost string

	query     string
	results   []store.Command
	cursor    int
	offset    int
	statusIdx int

	// Every query carries a sequence number; a result whose number is stale
	// belongs to a query the user has already typed past.
	seq int

	// The commands either side of the selected one in its shell session, which
	// together with the selected one read as a timeline.
	before       []store.Command
	after        []store.Command
	neighborsFor string

	width, height int
	copied        bool
	err           error

	// Chosen is the command the user picked, and the only thing printed to
	// stdout. Empty means they cancelled.
	Chosen string
}

type resultsMsg struct {
	seq  int
	cmds []store.Command
	err  error
}

type neighborsMsg struct {
	forID         string
	before, after []store.Command
}

type clearCopiedMsg struct{}

func New(st *store.Store, th *theme.Theme, initialQuery string) *Model {
	host, _ := os.Hostname()
	return &Model{
		store: st, theme: th, localHost: host,
		query: initialQuery, width: 80, height: 24,
	}
}

func (m *Model) Init() tea.Cmd {
	return m.runQuery()
}

func (m *Model) filter() store.Filter {
	return store.Filter{
		Text:   m.query,
		Status: statusCycle[m.statusIdx],
		Limit:  queryLimit,
	}
}

func (m *Model) runQuery() tea.Cmd {
	m.seq++
	seq := m.seq
	f := m.filter()
	st := m.store
	return func() tea.Msg {
		cmds, err := st.QueryCommands(f)
		return resultsMsg{seq: seq, cmds: cmds, err: err}
	}
}

// loadNeighbors fills the detail pane's session context. It is deliberately a
// separate round trip so the list paints without waiting on it.
func (m *Model) loadNeighbors(c store.Command) tea.Cmd {
	st := m.store
	return func() tea.Msg {
		before, err := st.SessionContext(c.SessionID, c.StartTime, sessionContextLines)
		if err != nil {
			return neighborsMsg{forID: c.ID}
		}
		after, err := st.SessionAfter(c.SessionID, c.StartTime, sessionContextLines)
		if err != nil {
			return neighborsMsg{forID: c.ID, before: before}
		}
		return neighborsMsg{forID: c.ID, before: before, after: after}
	}
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case resultsMsg:
		if msg.seq != m.seq {
			return m, nil // a query the user has already typed past
		}
		m.err = msg.err
		m.results = msg.cmds
		if m.cursor >= len(m.results) {
			m.cursor = max(0, len(m.results)-1)
		}
		m.clampOffset()
		return m, m.neighborCmd()

	case neighborsMsg:
		if msg.forID == m.selectedID() {
			m.before, m.after = msg.before, msg.after
			m.neighborsFor = msg.forID
		}
		return m, nil

	case clearCopiedMsg:
		m.copied = false
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c", "ctrl+g":
		return m, tea.Quit

	case "enter":
		if c := m.selected(); c != nil {
			m.Chosen = c.Command
		}
		return m, tea.Quit

	case "up", "ctrl+p":
		m.move(-1)
		return m, m.neighborCmd()

	case "down", "ctrl+n":
		m.move(1)
		return m, m.neighborCmd()

	case "pgup":
		m.move(-m.visibleItems())
		return m, m.neighborCmd()

	case "pgdown":
		m.move(m.visibleItems())
		return m, m.neighborCmd()

	case "ctrl+f":
		m.statusIdx = (m.statusIdx + 1) % len(statusCycle)
		m.cursor, m.offset = 0, 0
		return m, m.runQuery()

	case "ctrl+y":
		if c := m.selected(); c != nil {
			copyToClipboard(c.Command)
			m.copied = true
			return m, tea.Tick(1500*time.Millisecond, func(time.Time) tea.Msg { return clearCopiedMsg{} })
		}
		return m, nil

	case "ctrl+u":
		m.query = ""
		m.cursor, m.offset = 0, 0
		return m, m.runQuery()

	case "backspace":
		if m.query != "" {
			r := []rune(m.query)
			m.query = string(r[:len(r)-1])
			m.cursor, m.offset = 0, 0
			return m, m.runQuery()
		}
		return m, nil
	}

	if msg.Type == tea.KeyRunes {
		m.query += string(msg.Runes)
		m.cursor, m.offset = 0, 0
		return m, m.runQuery()
	}
	if msg.Type == tea.KeySpace {
		m.query += " "
		m.cursor, m.offset = 0, 0
		return m, m.runQuery()
	}
	return m, nil
}

func (m *Model) move(delta int) {
	if len(m.results) == 0 {
		return
	}
	m.cursor = min(max(m.cursor+delta, 0), len(m.results)-1)
	m.clampOffset()
}

func (m *Model) clampOffset() {
	visible := m.visibleItems()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+visible {
		m.offset = m.cursor - visible + 1
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func (m *Model) selected() *store.Command {
	if m.cursor < 0 || m.cursor >= len(m.results) {
		return nil
	}
	return &m.results[m.cursor]
}

func (m *Model) selectedID() string {
	if c := m.selected(); c != nil {
		return c.ID
	}
	return ""
}

// neighborCmd fetches session context only when the selection actually moved to
// a command we have not already loaded.
func (m *Model) neighborCmd() tea.Cmd {
	c := m.selected()
	if c == nil {
		m.before, m.after, m.neighborsFor = nil, nil, ""
		return nil
	}
	if m.neighborsFor == c.ID {
		return nil
	}
	m.before, m.after = nil, nil
	return m.loadNeighbors(*c)
}

// copyToClipboard uses OSC 52, which the terminal emulator handles — so it works
// over SSH and needs no clipboard binary on the box running the shell.
func copyToClipboard(s string) {
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return
	}
	defer tty.Close()
	fmt.Fprintf(tty, "\x1b]52;c;%s\x07", base64.StdEncoding.EncodeToString([]byte(s)))
}

// Run shows the picker and returns the chosen command, or "" if cancelled.
//
// The interface is drawn on /dev/tty rather than stdout so that the caller can
// capture the selection with $(...) while the UI still reaches the screen.
func Run(st *store.Store, initialQuery string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("shcr tui needs a terminal: %w", err)
	}
	defer tty.Close()

	// Opening the picker says two things: history is about to be read, and the
	// user is at the keyboard. Fired into the background so it cannot touch the
	// time to first paint, and ignored on failure — a picker that fails because
	// the daemon is down would be absurd.
	go func() { _ = ipc.Nudge("picker") }()

	// The theme renders for the tty, not for stdout. Under Ctrl+R stdout is the
	// shell's $(...) capture pipe, and a renderer bound to it reports a
	// colourless terminal — which silently discarded every style the picker had.
	m := New(st, theme.New(tty), initialQuery)
	p := tea.NewProgram(m,
		tea.WithInput(tty),
		tea.WithOutput(tty),
		tea.WithAltScreen(),
	)
	final, err := p.Run()
	if err != nil {
		return "", err
	}
	if fm, ok := final.(*Model); ok {
		return fm.Chosen, nil
	}
	return "", nil
}
