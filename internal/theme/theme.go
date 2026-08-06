// Package theme holds the one visual vocabulary shared by every surface that
// shows command history — the Ctrl+R picker and `shcr list` today, the web
// dashboard later.
//
// Everything here hangs off a lipgloss Renderer bound to a specific writer
// rather than the package-level default. That is not tidiness: the default
// renderer profiles os.Stdout, and under Ctrl+R stdout is the `$(...)` capture
// pipe, so the default renderer reports a colourless terminal and silently
// discards every style. Binding to the writer we actually draw on is what makes
// colour appear at all.
package theme

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/shellcrumbs/shcr/internal/store"
)

// Status colour is the only saturated hue on screen, and it means the same
// thing on every surface. Everything else borrows the terminal's own foreground
// so the output sits inside the user's theme instead of fighting it.
var (
	colorCompleted = lipgloss.AdaptiveColor{Light: "#1a7f37", Dark: "#3fb950"}
	colorRunning   = lipgloss.AdaptiveColor{Light: "#9a6700", Dark: "#d29922"}
	colorFailed    = lipgloss.AdaptiveColor{Light: "#cf222e", Dark: "#f85149"}
	colorOrphaned  = lipgloss.AdaptiveColor{Light: "#6e7781", Dark: "#8b949e"}

	colorAccent = lipgloss.AdaptiveColor{Light: "#0969da", Dark: "#58a6ff"}
	colorMuted  = lipgloss.AdaptiveColor{Light: "#6e7781", Dark: "#8b949e"}
	colorFrame  = lipgloss.AdaptiveColor{Light: "#d0d7de", Dark: "#30363d"}

	// A chip is a surface tint, not a hue — low enough contrast that a row of
	// them reads as one quiet block beside the command.
	colorChipBG = lipgloss.AdaptiveColor{Light: "#eaeef2", Dark: "#262c36"}
	colorChipFG = lipgloss.AdaptiveColor{Light: "#57606a", Dark: "#9aa5b1"}

	// The selected row is a band across the full width. A marker and bold text
	// alone were too easy to lose in a full screen of results — bold carries
	// almost no weight on a light terminal, and the eye has nothing to land on.
	// Still a surface rather than a hue, so it sits under the status colours
	// instead of competing with them.
	colorSelBG = lipgloss.AdaptiveColor{Light: "#dbe3ec", Dark: "#2d3542"}
)

type Theme struct {
	r *lipgloss.Renderer

	// sel is this theme again, with every style carrying the selection
	// background. A selected row is rendered entirely through it.
	//
	// Applying a background to the finished line instead does not work: each
	// inner style ends with a reset, which clears the background for everything
	// after it and leaves the band in stripes.
	sel *Theme
	// fill paints the gaps — padding, separators, the spaces between chips.
	// Without it the band would show the terminal through every space in the row.
	fill lipgloss.Style
	// hasBG marks the selection variant, so plain runs of text know whether they
	// need painting at all.
	hasBG bool

	Frame    lipgloss.Style
	Title    lipgloss.Style
	Muted    lipgloss.Style
	Accent   lipgloss.Style
	Match    lipgloss.Style
	Selected lipgloss.Style
	Label    lipgloss.Style
	Error    lipgloss.Style

	// chipInfo is worn by the host and duration chips alike: neutral text on
	// the chip surface. Only the exit code gets a colour of its own.
	chipInfo lipgloss.Style
	chipExit lipgloss.Style
}

// New builds a theme that renders for w.
//
// The terminal background is decided up front rather than detected. Detection
// costs an OSC 11 query with a five second timeout, and on the picker's
// tty-bound renderer that query would run after Bubble Tea has taken the
// terminal — the exact hang bubbletea's own source warns about. A wrong guess
// costs slightly off contrast; a hang costs seconds before the first frame.
func New(w io.Writer) *Theme { return NewWithMode(w, ColorAuto) }

// ColorMode overrides the automatic decision made from the writer and the
// environment.
type ColorMode int

const (
	// ColorAuto keeps termenv's behaviour: colour on a terminal, plain text when
	// piped or redirected, and off entirely when NO_COLOR is set.
	ColorAuto ColorMode = iota
	// ColorAlways forces colour even when the destination is not a terminal,
	// which is what `| less -R` needs.
	ColorAlways
	// ColorNever forces plain text.
	ColorNever
)

func ParseColorMode(s string) (ColorMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return ColorAuto, nil
	case "always", "force", "yes":
		return ColorAlways, nil
	case "never", "none", "no":
		return ColorNever, nil
	}
	return ColorAuto, fmt.Errorf("unknown colour mode %q (want auto, always or never)", s)
}

// NewWithMode is New with the colour decision overridden.
func NewWithMode(w io.Writer, mode ColorMode) *Theme {
	r := lipgloss.NewRenderer(w)
	r.SetHasDarkBackground(resolveDarkBackground(r))
	switch mode {
	case ColorNever:
		r.SetColorProfile(termenv.Ascii)
	case ColorAlways:
		if r.ColorProfile() == termenv.Ascii {
			r.SetColorProfile(termenv.TrueColor)
		}
	}
	return build(r)
}

func build(r *lipgloss.Renderer) *Theme {
	t := buildOn(r, nil)
	t.sel = buildOn(r, colorSelBG)
	return t
}

// buildOn builds the palette over an optional background. bg nil is the normal
// theme; non-nil is the selection variant.
func buildOn(r *lipgloss.Renderer, bg lipgloss.TerminalColor) *Theme {
	base := r.NewStyle()
	if bg != nil {
		base = base.Background(bg)
	}
	// On a selected row the chip keeps its shape but not its own surface: a
	// second tint inside the band reads as a hole punched in it.
	chipSurface := lipgloss.TerminalColor(colorChipBG)
	if bg != nil {
		chipSurface = bg
	}
	chip := base.Background(chipSurface).Padding(0, 1)

	return &Theme{
		r:        r,
		fill:     base,
		hasBG:    bg != nil,
		Frame:    base.Foreground(colorFrame),
		Title:    base.Foreground(colorAccent).Bold(true),
		Muted:    base.Foreground(colorMuted),
		Accent:   base.Foreground(colorAccent),
		Match:    base.Foreground(colorAccent).Bold(true),
		Selected: base.Bold(true),
		Label:    base.Foreground(colorMuted),
		Error:    base.Foreground(colorFailed).Bold(true),

		chipInfo: chip.Foreground(colorChipFG),
		chipExit: chip.Foreground(colorFailed),
	}
}

// body paints a plain run of text so it does not punch a hole in a selected
// row. On the normal theme it is the identity.
func (t *Theme) body(s string) string {
	if !t.hasBG || s == "" {
		return s
	}
	return t.fill.Render(s)
}

// padTo pads s out to w columns, painting the padding when there is a band to
// keep unbroken.
func (t *Theme) padTo(s string, w int) string {
	d := w - Width(s)
	if d <= 0 {
		return s
	}
	return s + t.body(strings.Repeat(" ", d))
}

// resolveDarkBackground prefers an explicit setting, then the COLORFGBG hint
// many terminals export, and otherwise assumes dark — which is what developer
// terminals overwhelmingly are.
func resolveDarkBackground(r *lipgloss.Renderer) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SHCR_THEME"))) {
	case "light":
		return false
	case "dark":
		return true
	case "auto":
		// Explicitly opted into the query, with its timeout, at the caller's risk.
		return r.HasDarkBackground()
	}
	if fgbg := os.Getenv("COLORFGBG"); fgbg != "" {
		if fields := strings.Split(fgbg, ";"); len(fields) >= 2 {
			if bg, err := strconv.Atoi(fields[len(fields)-1]); err == nil {
				// The usual convention: 0-6 and 8 are the dark end of the
				// 16-colour palette.
				return bg <= 6 || bg == 8
			}
		}
	}
	return true
}

// StatusColor is the shared mapping every surface uses, so a failure is the same
// red in the picker, in `shcr list` and eventually in the dashboard.
func StatusColor(status string) lipgloss.TerminalColor {
	switch status {
	case store.StatusCompleted:
		return colorCompleted
	case store.StatusRunning:
		return colorRunning
	case store.StatusFailed:
		return colorFailed
	case store.StatusOrphaned:
		return colorOrphaned
	}
	return colorMuted
}

func StatusGlyph(status string) string {
	switch status {
	case store.StatusCompleted:
		return "✓"
	case store.StatusRunning:
		return "●"
	case store.StatusFailed:
		return "✗"
	case store.StatusOrphaned:
		return "◌"
	}
	return "·"
}

// Dot is the status indicator: one glyph, one saturated colour, the only strong
// colour in a row.
func (t *Theme) Dot(status string) string {
	// fill rather than a bare style off the renderer: on a selected row this is
	// the one glyph that would otherwise sit in a hole in the band.
	return t.fill.Foreground(StatusColor(status)).Render(StatusGlyph(status))
}

// ImportedGlyph marks a command recovered from a shell's history file.
const ImportedGlyph = "↧"

// Mark is the indicator for one row. An imported command gets its own muted
// glyph rather than a green tick: nothing watched it run, its exit code is
// unknown, and a tick would be claiming otherwise.
func (t *Theme) Mark(c store.Command) string {
	if c.Imported {
		return t.Muted.Render(ImportedGlyph)
	}
	return t.Dot(c.Status)
}

// Width reports the display width of a possibly-styled string.
func Width(s string) int { return lipgloss.Width(s) }
