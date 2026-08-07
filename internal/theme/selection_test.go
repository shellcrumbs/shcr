package theme

import (
	"bytes"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/shellcrumbs/shcr/internal/store"
)

// colourTheme forces a colour profile, because the writer in a test is not a
// terminal and would otherwise render everything as plain text — which would
// make every assertion below pass without testing anything.
func colourTheme(t *testing.T) *Theme {
	t.Helper()
	r := lipgloss.NewRenderer(&bytes.Buffer{})
	r.SetHasDarkBackground(true)
	r.SetColorProfile(termenv.TrueColor)
	return build(r)
}

var sgr = regexp.MustCompile(`\x1b\[([0-9;]*)m`)

// bareColumns walks a rendered line and returns the visible characters that are
// drawn with no background set. On a selected row there must be none: each one
// is a place where the terminal shows through the band.
func bareColumns(line string) string {
	var bare strings.Builder
	bg := false
	last := 0
	for _, m := range sgr.FindAllStringSubmatchIndex(line, -1) {
		text := line[last:m[0]]
		if !bg {
			bare.WriteString(text)
		}
		for _, p := range strings.Split(line[m[2]:m[3]], ";") {
			switch {
			case p == "" || p == "0":
				bg = false // reset clears the background too
			case strings.HasPrefix(p, "4") && len(p) >= 2:
				bg = true // 48;2;r;g;b and the 40-47 range
			}
		}
		last = m[1]
	}
	if !bg {
		bare.WriteString(line[last:])
	}
	return bare.String()
}

func sampleCommand() store.Command {
	exit, dur := 127, int64(2400)
	branch := "main"
	return store.Command{
		ID: "c1", Command: "npm run build:prod", Hostname: "build-server",
		Cwd: "/home/u/app", GitBranch: &branch, Shell: "bash",
		StartTime: 1_700_000_000_000, ExitCode: &exit, DurationMS: &dur,
		Status: store.StatusFailed, SessionID: "s1",
	}
}

// The band has to run the full width without a gap. Padding, the spaces between
// chips and any text that carries no style of its own are all places where the
// terminal background would otherwise show through, and a striped highlight
// looks like a rendering fault rather than a cursor.
func TestSelectedRowIsOneUnbrokenBand(t *testing.T) {
	th := colourTheme(t)
	for _, width := range []int{40, 60, 80, 120, 200} {
		line := th.Row(sampleCommand(), RowOpts{
			Width: width, Selected: true, Tokens: []string{"npm"},
			LocalHost: "laptop", Now: 1_700_000_100_000, ShowAge: true, ReserveHost: true,
		})
		if bare := bareColumns(line); bare != "" {
			t.Errorf("width %d: %q is drawn without a background", width, bare)
		}
	}
}

// A row that is not selected must not be painted at all, or every row would
// look selected.
func TestAnUnselectedRowHasNoBand(t *testing.T) {
	th := colourTheme(t)
	line := th.Row(sampleCommand(), RowOpts{Width: 80, Tokens: []string{"npm"}, LocalHost: "laptop"})
	if bareColumns(line) == "" {
		t.Error("an unselected row is fully painted, so every row reads as the cursor")
	}
}

// Whatever the highlight costs, it cannot cost a column: the picker draws these
// inside a bordered pane, and one column over corrupts the frame.
func TestSelectionDoesNotChangeRowWidth(t *testing.T) {
	th := colourTheme(t)
	for _, width := range []int{20, 40, 60, 80, 120, 200} {
		for _, sel := range []bool{false, true} {
			line := th.Row(sampleCommand(), RowOpts{
				Width: width, Selected: sel, Tokens: []string{"npm"},
				LocalHost: "laptop", Now: 1_700_000_100_000, ShowAge: true, ReserveHost: true,
			})
			if got := Width(line); got != width {
				t.Errorf("width %d, selected=%v: rendered %d columns", width, sel, got)
			}
		}
	}
}

// The band is a surface, not a hue. A failure has to stay red on the selected
// row, or the one row you are looking at is the one that stops telling you what
// happened.
func TestStatusColourSurvivesTheBand(t *testing.T) {
	th := colourTheme(t)
	plain := th.Row(sampleCommand(), RowOpts{Width: 80, LocalHost: "laptop"})
	sel := th.Row(sampleCommand(), RowOpts{Width: 80, Selected: true, LocalHost: "laptop"})

	// The foreground parameters, not the whole sequence: on a selected row
	// lipgloss emits the foreground and the background in one SGR, so the
	// sequences differ even though the colour is the same.
	fg := regexp.MustCompile(`38;2;\d+;\d+;\d+`).FindString(th.Dot(store.StatusFailed))
	if fg == "" {
		t.Fatal("no colour in the status dot; the profile is not set")
	}
	if !strings.Contains(plain, fg) {
		t.Fatalf("premise gone: the plain row has no status colour either")
	}
	if !strings.Contains(sel, fg) {
		t.Errorf("the status colour is missing from the selected row")
	}
}

func TestWrapBreaksOnWordsWhereItCan(t *testing.T) {
	got := Wrap("cd ~/domjudge-scoreboard && npm run dev", 34, 0)
	for _, line := range got {
		if Width(line) > 34 {
			t.Fatalf("line over width: %q", line)
		}
	}
	joined := strings.Join(got, "|")
	if strings.Contains(joined, "ru|n ") {
		t.Errorf("wrapped mid-word: %v", got)
	}
	// Nothing may be lost or invented on the way through.
	if flat := strings.ReplaceAll(strings.Join(got, ""), " ", ""); flat !=
		strings.ReplaceAll("cd ~/domjudge-scoreboard && npm run dev", " ", "") {
		t.Errorf("text changed: %v", got)
	}
}

// A word with no break in it still has to be cut, or it would run out of the
// pane. Paths and URLs are the common case.
func TestWrapStillBreaksAWordTooLongForALine(t *testing.T) {
	long := "/home/u/very/deeply/nested/path/that/keeps/going/without/a/single/space"
	got := Wrap(long, 20, 0)
	if len(got) < 2 {
		t.Fatalf("did not wrap at all: %v", got)
	}
	for _, line := range got {
		if Width(line) > 20 {
			t.Errorf("line over width: %q", line)
		}
	}
	if strings.Join(got, "") != long {
		t.Errorf("text changed:\n got  %q\n want %q", strings.Join(got, ""), long)
	}
}

func TestWrapHonoursMaxLines(t *testing.T) {
	got := Wrap(strings.Repeat("word ", 100), 20, 3)
	if len(got) != 3 {
		t.Errorf("got %d lines, want 3", len(got))
	}
}

func TestParseOSC11(t *testing.T) {
	for _, tc := range []struct {
		in   string
		dark bool
		ok   bool
	}{
		{"\x1b]11;rgb:1e1e/1e1e/1e1e\a", true, true},
		{"\x1b]11;rgb:fdfd/f6f6/e3e3\x1b\\", false, true}, // a cream terminal
		{"\x1b]11;rgb:ff/ff/ff\a", false, true},           // 8-bit components
		{"\x1b]11;rgb:0/0/0\a", true, true},               // 4-bit components
		{"\x1b]11;rgb:1e1e/1e1e\a", false, false},         // short
		{"", false, false},
		{"garbage", false, false},
	} {
		dark, ok := parseOSC11(tc.in)
		if dark != tc.dark || ok != tc.ok {
			t.Errorf("%q -> dark=%v ok=%v, want dark=%v ok=%v", tc.in, dark, ok, tc.dark, tc.ok)
		}
	}
}

// luminance and contrast implement WCAG 2.1, which is the only non-negotiable
// thing about a colour pair: whatever the band looks like, the text on it has
// to be legible.
func luminance(hex string) float64 {
	ch := func(s string) float64 {
		v, _ := strconv.ParseUint(s, 16, 32)
		c := float64(v) / 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	h := strings.TrimPrefix(hex, "#")
	return 0.2126*ch(h[0:2]) + 0.7152*ch(h[2:4]) + 0.0722*ch(h[4:6])
}

func contrast(a, b string) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// The band paints its own background, so it has to bring its own foreground.
// The pair has to be legible in both resolutions — including the one reached by
// guessing wrong, which is the case that produced a dark band under dark text.
func TestTheBandsTextIsLegibleOnIt(t *testing.T) {
	for _, tc := range []struct{ mode, bg, fg string }{
		{"light", colorSelBG.Light, colorSelFG.Light},
		{"dark", colorSelBG.Dark, colorSelFG.Dark},
	} {
		if got := contrast(tc.bg, tc.fg); got < 4.5 {
			t.Errorf("%s band: %s on %s is %.1f:1, want at least 4.5:1", tc.mode, tc.fg, tc.bg, got)
		}
	}
	// And the status colours still have to read against the band, since that is
	// the row whose result you are looking at.
	for _, tc := range []struct {
		mode, bg string
		fg       []string
	}{
		{"light", colorSelBG.Light, []string{colorFailed.Light, colorCompleted.Light, colorRunning.Light, colorMuted.Light}},
		{"dark", colorSelBG.Dark, []string{colorFailed.Dark, colorCompleted.Dark, colorRunning.Dark, colorMuted.Dark}},
	} {
		for _, fg := range tc.fg {
			if got := contrast(tc.bg, fg); got < 3 {
				t.Errorf("%s band: %s on %s is %.1f:1, want at least 3:1", tc.mode, fg, tc.bg, got)
			}
		}
	}
}

// Redirected output has no width to fit into, so the command has to survive
// whole: `shcr list | grep` on a long command should find it.
func TestAnUnpaddedRowKeepsTheWholeCommand(t *testing.T) {
	th := colourTheme(t)
	long := "docker run --rm -v /very/long/path/that/keeps/going:/mnt " +
		"--env SOMETHING=else --entrypoint /bin/sh image:tag -c 'echo hello world'"
	c := sampleCommand()
	c.Command = long

	line := th.Row(c, RowOpts{Width: 100, Unpadded: true, LocalHost: "laptop"})
	if !strings.Contains(line, long) {
		t.Errorf("the command was cut:\n %q", line)
	}
	if strings.Contains(line, "…") {
		t.Errorf("an ellipsis survived into unpadded output: %q", line)
	}
	// And no run of padding: the metadata sits next to the command, not at
	// column 100.
	if strings.Contains(line, "     ") {
		t.Errorf("padded anyway: %q", line)
	}
	// The bounded row still truncates, which is what a terminal needs.
	bounded := th.Row(c, RowOpts{Width: 100, LocalHost: "laptop"})
	if Width(bounded) != 100 {
		t.Errorf("bounded row is %d columns, want 100", Width(bounded))
	}
}
