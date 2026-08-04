package web

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

var (
	cssVarDecl  = regexp.MustCompile(`(?m)^\s*(--[a-z-]+):\s*([^;]+);`)
	lightBlock  = regexp.MustCompile(`(?s)@media \(prefers-color-scheme: light\) \{.*?\n\s*\}\n\}`)
	isColour    = regexp.MustCompile(`^(#|rgba?\()`)
	rootBlockRe = regexp.MustCompile(`(?sm)^:root \{.*?\n\}`)
)

// Every colour goes through a variable, and every variable must carry both a
// light and a dark value. A colour added with only one would silently render a
// dark value on a light page — on somebody else's machine, not the author's.
func TestEveryColourDefinesBothThemes(t *testing.T) {
	css, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	root := rootBlockRe.Find(css)
	if root == nil {
		t.Fatal("no :root block")
	}
	for _, m := range cssVarDecl.FindAllStringSubmatch(string(root), -1) {
		name, value := m[1], strings.TrimSpace(m[2])
		if !isColour.MatchString(value) && !strings.HasPrefix(value, "light-dark(") {
			continue // --mono, --sans, --radius: nothing to theme
		}
		if !strings.HasPrefix(value, "light-dark(") {
			t.Errorf("%s is a bare colour (%s); it needs a light-dark() pair", name, value)
		}
	}
}

// The toggle works by changing color-scheme, which re-resolves every
// light-dark() above it. Without these the button would set an attribute that
// nothing reads.
func TestThemeOverridesExist(t *testing.T) {
	css, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	text := string(css)
	root := string(rootBlockRe.Find(css))
	if !strings.Contains(root, "color-scheme: light dark") {
		t.Error(":root should follow the system by default (color-scheme: light dark)")
	}
	for _, want := range []string{
		`:root[data-theme="light"]`,
		`:root[data-theme="dark"]`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing the %s override", want)
		}
	}
}

// The preference is applied from a blocking script in <head>. In app.js — which
// loads at the end of <body> — it would run after the first paint, flashing the
// wrong theme on every load for anyone whose choice differs from their system.
// An inline script would be the usual fix and the CSP forbids it.
func TestThemePreferenceIsAppliedBeforePaint(t *testing.T) {
	html, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	text := string(html)
	head := text[:strings.Index(text, "</head>")]
	if !strings.Contains(head, `<script src="/theme.js">`) {
		t.Error("theme.js must load from <head>, before anything is painted")
	}
	if strings.Contains(head, `src="/theme.js" defer`) || strings.Contains(head, `src="/theme.js" async`) {
		t.Error("theme.js must not be deferred; that is the flash it exists to prevent")
	}
	js, err := os.ReadFile("static/theme.js")
	if err != nil {
		t.Fatal(err)
	}
	// It runs before the CSS is known to have applied, so it must touch nothing
	// but the attribute.
	if strings.Contains(string(js), "fetch(") || strings.Contains(string(js), "document.body") {
		t.Error("theme.js should only set the attribute")
	}
}

// The shortcuts are only worth having if they can be found. Nothing in the page
// hinted at the two that already existed, which is why nobody used them.
func TestKeyboardShortcutsAreDiscoverable(t *testing.T) {
	html, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(html)
	if !strings.Contains(page, `class="search-hint"`) {
		t.Error(`the search box should advertise "/"`)
	}
	if !strings.Contains(page, `id="shortcuts"`) {
		t.Error("no shortcuts overlay")
	}
	for _, key := range []string{"/", "j", "k", "Enter", "Esc", "?"} {
		if !strings.Contains(page, "<kbd>"+key+"</kbd>") {
			t.Errorf("the overlay does not list %q", key)
		}
	}

	js, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	code := string(js)
	for _, k := range []string{`"Escape"`, `"ArrowDown"`, `"ArrowUp"`, `"j"`, `"k"`, `"/"`, `"?"`} {
		if !strings.Contains(code, k) {
			t.Errorf("no handler for %s", k)
		}
	}
	// A letter shortcut that fires while typing makes the search box unusable.
	if !strings.Contains(code, "isTyping") {
		t.Error("letter shortcuts must be suppressed inside text fields")
	}
}
