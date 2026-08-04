package theme

import (
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/shellcrumbs/shcr/internal/store"
)

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func plain(s string) string { return ansi.ReplaceAllString(s, "") }

// plainTheme renders without colour, so tests can assert on layout.
func plainTheme(t *testing.T) *Theme {
	t.Helper()
	return NewWithMode(io.Discard, ColorNever)
}

// colorTheme forces colour on despite writing to a non-terminal.
func colorTheme(t *testing.T) *Theme {
	t.Helper()
	return NewWithMode(io.Discard, ColorAlways)
}

func cmd(mods ...func(*store.Command)) store.Command {
	exit := 0
	dur := int64(2400)
	c := store.Command{
		ID: "c1", Command: "npm run build:prod", Hostname: "laptop-mac",
		Cwd: "/home/u/app", Status: store.StatusCompleted,
		StartTime: time.Now().UnixMilli(), ExitCode: &exit, DurationMS: &dur,
	}
	for _, m := range mods {
		m(&c)
	}
	return c
}

func TestRowIsExactlyTheRequestedWidth(t *testing.T) {
	th := plainTheme(t)
	for _, w := range []int{40, 60, 80, 100, 120, 200} {
		for _, o := range []RowOpts{
			{Width: w},
			{Width: w, Selected: true},
			{Width: w, ShowTime: true, BaseCwd: "/elsewhere", LocalHost: "other-host"},
			{Width: w, ShowTime: true, Tokens: []string{"npm"}},
		} {
			got := th.Row(cmd(), o)
			if Width(got) != w {
				t.Errorf("width %d opts %+v: rendered %d columns\n  %q", w, o, Width(got), plain(got))
			}
		}
	}
}

func TestHostChipOnlyWhenDifferent(t *testing.T) {
	th := plainTheme(t)

	same := th.Row(cmd(), RowOpts{Width: 100, LocalHost: "laptop-mac"})
	if strings.Contains(plain(same), "laptop-mac") {
		t.Errorf("host shown for a command that ran here:\n  %q", plain(same))
	}

	remote := th.Row(cmd(func(c *store.Command) { c.Hostname = "build-server" }),
		RowOpts{Width: 100, LocalHost: "laptop-mac"})
	if !strings.Contains(plain(remote), "build-server") {
		t.Errorf("host missing for a command from another machine:\n  %q", plain(remote))
	}

	// With no local host known, the column stays empty rather than guessing.
	unknown := th.Row(cmd(), RowOpts{Width: 100})
	if strings.Contains(plain(unknown), "laptop-mac") {
		t.Errorf("host shown with no LocalHost set:\n  %q", plain(unknown))
	}
}

func TestExitChipOnlyWhenNonZero(t *testing.T) {
	th := plainTheme(t)

	ok := plain(th.Row(cmd(), RowOpts{Width: 100}))
	if strings.Contains(ok, " 0 ") {
		t.Errorf("a zero exit code should not take up space:\n  %q", ok)
	}

	failed := plain(th.Row(cmd(func(c *store.Command) {
		e := 127
		c.ExitCode = &e
		c.Status = store.StatusFailed
	}), RowOpts{Width: 100}))
	if !strings.Contains(failed, "127") {
		t.Errorf("non-zero exit code missing:\n  %q", failed)
	}

	// Running and orphaned commands have no exit code at all.
	running := plain(th.Row(cmd(func(c *store.Command) {
		c.ExitCode = nil
		c.DurationMS = nil
		c.Status = store.StatusRunning
	}), RowOpts{Width: 100}))
	if strings.ContainsAny(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(running), "●")), "0123456789") {
		t.Errorf("a running command should carry no numeric chips:\n  %q", running)
	}
}

func TestDurationChipOnlyWhenKnown(t *testing.T) {
	th := plainTheme(t)
	if got := plain(th.Row(cmd(), RowOpts{Width: 100})); !strings.Contains(got, "2.4s") {
		t.Errorf("duration missing:\n  %q", got)
	}
	got := plain(th.Row(cmd(func(c *store.Command) { c.DurationMS = nil }), RowOpts{Width: 100}))
	if strings.Contains(got, "s ") && strings.Contains(got, "2.4") {
		t.Errorf("duration shown when unknown:\n  %q", got)
	}
}

func TestCwdOnlyWhenDifferentFromBase(t *testing.T) {
	th := plainTheme(t)

	same := plain(th.Row(cmd(), RowOpts{Width: 100, BaseCwd: "/home/u/app"}))
	if strings.Contains(same, "app") && strings.Contains(same, "~/") {
		t.Errorf("cwd shown for a command run in the base directory:\n  %q", same)
	}

	elsewhere := plain(th.Row(cmd(func(c *store.Command) { c.Cwd = "/home/u/other" }),
		RowOpts{Width: 100, BaseCwd: "/home/u/app"}))
	if !strings.Contains(elsewhere, "other") {
		t.Errorf("cwd missing for a command run elsewhere:\n  %q", elsewhere)
	}

	// The picker passes no base, and so never shows a directory.
	picker := plain(th.Row(cmd(func(c *store.Command) { c.Cwd = "/home/u/other" }), RowOpts{Width: 100}))
	if strings.Contains(picker, "other") {
		t.Errorf("cwd shown with no BaseCwd set:\n  %q", picker)
	}
}

// As the terminal narrows, metadata gives way to the command rather than the
// other way round.
func TestChipsDropRightToLeftWhenNarrow(t *testing.T) {
	th := plainTheme(t)
	c := cmd(func(x *store.Command) {
		x.Hostname = "build-server"
		x.Cwd = "/home/u/somewhere/deep"
		e := 127
		x.ExitCode = &e
		x.Status = store.StatusFailed
	})
	o := RowOpts{ShowTime: true, LocalHost: "laptop", BaseCwd: "/home/u/app"}

	widths := []int{110, 90, 70, 50, 36}
	lastCount := 99
	for _, w := range widths {
		o.Width = w
		got := plain(th.Row(c, o))
		if Width(th.Row(c, o)) != w {
			t.Fatalf("width %d: rendered %d columns", w, Width(th.Row(c, o)))
		}
		count := 0
		for _, marker := range []string{"somewhere", "build-server", "2.4s", "127"} {
			if strings.Contains(got, marker) {
				count++
			}
		}
		if count > lastCount {
			t.Errorf("narrowing to %d added metadata back (%d -> %d):\n  %q", w, lastCount, count, got)
		}
		lastCount = count

		// Whatever is dropped, some of the command must survive.
		if !strings.Contains(got, "npm") {
			t.Errorf("width %d: the command itself was squeezed out:\n  %q", w, got)
		}
	}
	// The exit code is the last thing standing.
	o.Width = 36
	if !strings.Contains(plain(th.Row(c, o)), "127") {
		t.Errorf("the exit code should outlive the other chips:\n  %q", plain(th.Row(c, o)))
	}
}

func TestNoColorRendersCleanTextAndKeepsAlignment(t *testing.T) {
	th := plainTheme(t)
	got := th.Row(cmd(func(c *store.Command) { c.Hostname = "build-server" }),
		RowOpts{Width: 90, ShowTime: true, LocalHost: "laptop", Tokens: []string{"npm"}, Selected: true})

	if strings.Contains(got, "\x1b") {
		t.Fatalf("escape sequences leaked into plain output: %q", got)
	}
	if Width(got) != 90 {
		t.Fatalf("plain row is %d columns, want 90", Width(got))
	}
	// Chips still read as separate tokens without their background colour.
	if !strings.Contains(got, "build-server") || !strings.Contains(got, "2.4s") {
		t.Errorf("metadata lost without colour: %q", got)
	}
}

func TestColorModeForcesOutputOnANonTerminal(t *testing.T) {
	// This is the regression that matters: the picker writes to a tty while its
	// stdout is a pipe, and styles must not silently degrade to plain text.
	th := colorTheme(t)
	got := th.Row(cmd(), RowOpts{Width: 80})
	if !strings.Contains(got, "\x1b") {
		t.Fatalf("ColorAlways produced no escape sequences: %q", got)
	}
	if Width(got) != 80 {
		t.Fatalf("coloured row is %d columns, want 80", Width(got))
	}
}

func TestMultilineCommandCollapsedToOneRow(t *testing.T) {
	th := plainTheme(t)
	got := th.Row(cmd(func(c *store.Command) { c.Command = "cat <<'EOF' > f\n line\nEOF" }),
		RowOpts{Width: 80})
	if strings.Contains(got, "\n") {
		t.Fatalf("row contains a newline: %q", got)
	}
	if !strings.Contains(got, "↵") {
		t.Errorf("continuation not marked: %q", got)
	}
}

func TestHighlightMarksMatchesWithoutChangingWidth(t *testing.T) {
	th := colorTheme(t)
	with := th.Row(cmd(), RowOpts{Width: 80, Tokens: []string{"npm", "build"}})
	without := th.Row(cmd(), RowOpts{Width: 80})
	if Width(with) != Width(without) {
		t.Fatalf("highlighting changed the width: %d vs %d", Width(with), Width(without))
	}
	if plain(with) != plain(without) {
		t.Fatalf("highlighting changed the text:\n  %q\n  %q", plain(with), plain(without))
	}
}

// Case folding does not preserve byte length: U+212A KELVIN SIGN is three
// bytes and folds to a one-byte "k". Searching the folded text and then using
// those offsets against the original shifts every later match, so the wrong
// span is highlighted and a slice can land inside a UTF-8 sequence.
func TestHighlightIsCorrectWhenFoldingChangesLength(t *testing.T) {
	th := colorTheme(t)
	for _, tc := range []struct{ text, token, want string }{
		{"Kelvin grep foo", "grep", "grep"},
		{"KKK git push", "push", "push"},
		{"GREP it", "grep", "GREP"},
		{"café npm test", "npm", "npm"},
	} {
		out := th.Highlight(tc.text, []string{tc.token})
		if plain(out) != tc.text {
			t.Errorf("Highlight(%q, %q) changed the text: %q", tc.text, tc.token, plain(out))
		}
		if !strings.Contains(out, th.Match.Render(tc.want)) {
			t.Errorf("Highlight(%q, %q) did not mark %q: %q", tc.text, tc.token, tc.want, out)
		}
	}
}

// A recorded command can contain any byte, and with sync it was recorded on
// someone else's machine. Escape sequences reaching the terminal let it clear
// the screen, and a carriage return makes the row show something other than
// what is stored — in the picker, that is the text about to land in the prompt.
func TestControlSequencesNeverReachTheTerminal(t *testing.T) {
	th := plainTheme(t)
	for _, tc := range []struct{ name, command, mustNotContain string }{
		{"clear screen", "echo \x1b[2Jgone", "\x1b"},
		{"carriage return rewrite", "echo before\rAFTER", "\r"},
		{"bell", "echo \a", "\a"},
		{"eight-bit CSI", "echo \u009b2J", "\u009b"},
		{"backspace", "echo a\bb", "\b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := th.Row(cmd(func(c *store.Command) { c.Command = tc.command }), RowOpts{Width: 80})
			if strings.Contains(row, tc.mustNotContain) {
				t.Errorf("row still carries %q: %q", tc.mustNotContain, row)
			}
			if Width(row) != 80 {
				t.Errorf("row is %d columns, want 80: %q", Width(row), row)
			}
		})
	}

	// Ordinary text is untouched.
	if got := Safe("git commit -m 'héllo wörld → ✓'"); got != "git commit -m 'héllo wörld → ✓'" {
		t.Errorf("Safe altered ordinary text: %q", got)
	}
	if got := SafeMultiline("a\nb"); got != "a\nb" {
		t.Errorf("SafeMultiline dropped a newline: %q", got)
	}
	if got := Safe("a\nb"); strings.Contains(got, "\n") {
		t.Errorf("Safe kept a newline: %q", got)
	}
}

// The point of the fixed slots is a column you can read down, which only holds
// if the widest thing that can land in one still fits. `43m 34s` in a chip is
// nine columns against a slot of eight, so every row with a duration over ten
// minutes used to shove the command text one column left of its neighbours —
// invisible until running commands started showing elapsed time.
func TestSlotsHoldTheWidestValueTheyCanCarry(t *testing.T) {
	th := plainTheme(t)
	var width int
	for _, ms := range []int64{1, 999, 8_200, 59_900, 72_000, 2_614_000, 3_599_000, 360_000_000} {
		row := th.Row(cmd(func(c *store.Command) {
			c.Command = "x"
			c.DurationMS = &ms
			c.Hostname = "build-server"
		}), RowOpts{Width: 100, LocalHost: "laptop", ReserveHost: true})
		at := strings.Index(plain(row), "build-server")
		if at < 0 {
			t.Fatalf("%v: host chip missing from %q", time.Duration(ms)*time.Millisecond, plain(row))
		}
		if width == 0 {
			width = at
			continue
		}
		if at != width {
			t.Errorf("duration %s pushed the host column to %d, other rows have it at %d:\n  %s",
				Duration(ms), at, width, plain(row))
		}
	}
}

func TestAgeStaysInItsColumn(t *testing.T) {
	const now = int64(1_800_000_000_000)
	for _, tc := range []struct {
		ago  time.Duration
		want string
	}{
		{10 * time.Second, "now"},
		{90 * time.Second, "1m"},
		{59 * time.Minute, "59m"},
		{3 * time.Hour, "3h"},
		{23 * time.Hour, "23h"},
		{6 * 24 * time.Hour, "6d"},
		{30 * 24 * time.Hour, "4w"},
		{400 * 24 * time.Hour, "1y"},
	} {
		got := Age(now-tc.ago.Milliseconds(), now)
		if got != tc.want {
			t.Errorf("%s ago -> %q, want %q", tc.ago, got, tc.want)
		}
		if Width(got) > ageSlot-1 {
			t.Errorf("%q is %d columns, too wide for the slot", got, Width(got))
		}
	}
	// A peer whose clock runs ahead reports the future; a negative age would be
	// worse than saying now.
	if got := Age(now+60_000, now); got != "now" {
		t.Errorf("a future timestamp rendered as %q", got)
	}
	if got := Age(0, now); got != "" {
		t.Errorf("a missing timestamp rendered as %q", got)
	}
}

// The age column must not eat into what it sits beside.
func TestAgeDoesNotDisturbTheOtherColumns(t *testing.T) {
	th := plainTheme(t)
	const now = int64(1_800_000_000_000)
	base := cmd(func(c *store.Command) { c.Command = "x"; c.Hostname = "build-server" })
	base.StartTime = now - int64(3*time.Hour/time.Millisecond)

	with := th.Row(base, RowOpts{Width: 100, LocalHost: "laptop", Now: now, ShowAge: true})
	without := th.Row(base, RowOpts{Width: 100, LocalHost: "laptop", Now: now})
	if Width(with) != 100 || Width(without) != 100 {
		t.Fatalf("widths: with=%d without=%d, want 100", Width(with), Width(without))
	}
	if !strings.Contains(plain(with), "3h") {
		t.Errorf("age missing: %q", plain(with))
	}
	if strings.Contains(plain(without), "3h") {
		t.Errorf("age shown without ShowAge: %q", plain(without))
	}
	// The host column keeps its place; only the command gives up room.
	if a, b := strings.Index(plain(with), "build-server"), strings.Index(plain(without), "build-server"); a >= b {
		t.Errorf("host moved right (%d -> %d) instead of the command giving up room", b, a)
	}
}

func TestStatusVocabularyIsShared(t *testing.T) {
	for _, s := range []string{
		store.StatusCompleted, store.StatusRunning, store.StatusFailed, store.StatusOrphaned,
	} {
		if StatusGlyph(s) == "·" {
			t.Errorf("no glyph defined for status %q", s)
		}
		if StatusColor(s) == nil {
			t.Errorf("no colour defined for status %q", s)
		}
	}
	if StatusGlyph("nonsense") != "·" {
		t.Error("an unknown status should fall back rather than panic")
	}
}

func TestTinyWidthDoesNotPanic(t *testing.T) {
	th := plainTheme(t)
	for _, w := range []int{0, -5, 1, 2, 3, 8} {
		got := th.Row(cmd(), RowOpts{Width: w, ShowTime: true})
		if w <= 0 {
			if got != "" {
				t.Errorf("width %d should render nothing, got %q", w, got)
			}
			continue
		}
		if Width(got) != w {
			t.Errorf("width %d rendered %d columns: %q", w, Width(got), got)
		}
	}
}

func TestProfileNeverQueriesTheTerminal(t *testing.T) {
	// A theme built for a plain writer must not block; if it ever starts issuing
	// OSC queries this test will hang rather than fail, which is the signal.
	done := make(chan struct{})
	go func() {
		defer close(done)
		th := New(io.Discard)
		_ = th.Row(cmd(), RowOpts{Width: 80})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("building a theme blocked, most likely on a terminal query")
	}
}

func TestParseColorMode(t *testing.T) {
	for in, want := range map[string]ColorMode{
		"": ColorAuto, "auto": ColorAuto, "always": ColorAlways,
		"ALWAYS": ColorAlways, "never": ColorNever, " none ": ColorNever,
	} {
		got, err := ParseColorMode(in)
		if err != nil || got != want {
			t.Errorf("ParseColorMode(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseColorMode("chartreuse"); err == nil {
		t.Error("an unknown mode should be an error")
	}
}

// A CJK glyph or an emoji is two columns wide. Counting runes instead of
// columns let rows render wider than their pane and pushed the picker's border
// off the screen.
func TestWideCharactersNeverOverflowTheRow(t *testing.T) {
	th := plainTheme(t)
	dur := int64(1200)
	texts := map[string]string{
		"cjk":        `git commit -m "修复了一个非常严重的问题，这个问题影响了所有用户"`,
		"emoji":      `git commit -m "🚀🚀🚀 ship it 🎉🎉🎉 done"`,
		"mixed":      "echo 日本語 and English mixed テキスト here for testing",
		"wide+ascii": "kubectl describe pod 中文名字-deployment-abcdef -n production",
		"combining":  "echo éééééé combining marks",
	}
	for name, text := range texts {
		for _, w := range []int{30, 40, 60, 80, 100, 140} {
			c := cmd(func(x *store.Command) { x.Command = text; x.DurationMS = &dur })
			for _, o := range []RowOpts{
				{Width: w},
				{Width: w, ShowTime: true, Selected: true},
				{Width: w, ShowTime: true, LocalHost: "other", BaseCwd: "/elsewhere"},
			} {
				if got := th.Row(c, o); Width(got) != w {
					t.Errorf("%s at width %d rendered %d columns:\n  %q", name, w, Width(got), got)
				}
			}
		}
	}
}

func TestTruncateAndTailPathCountColumns(t *testing.T) {
	// Ten CJK glyphs are twenty columns.
	if got := Truncate("你好世界你好世界你好", 10); Width(got) > 10 {
		t.Errorf("Truncate is %d columns wide: %q", Width(got), got)
	}
	if got := TailPath("/home/u/项目/深层/目录/结构", 12); Width(got) > 12 {
		t.Errorf("TailPath is %d columns wide: %q", Width(got), got)
	}
	// A grapheme cluster must never be split down the middle.
	for w := 1; w <= 12; w++ {
		got := Truncate("👨‍👩‍👧‍👦 family", w)
		if Width(got) > w {
			t.Errorf("Truncate(family emoji, %d) is %d columns: %q", w, Width(got), got)
		}
	}
	for _, line := range Wrap("日本語のテキストがここにあります", 8, 4) {
		if Width(line) > 8 {
			t.Errorf("Wrap produced a %d-column line: %q", Width(line), line)
		}
	}
}
