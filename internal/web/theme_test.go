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

// Every colour in the stylesheet goes through a variable, so the light theme is
// exactly the set of overrides. A colour added to :root and forgotten in the
// light block does not fail loudly — it just renders a dark value on a light
// page, on somebody else's machine.
func TestLightThemeOverridesEveryColour(t *testing.T) {
	css, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	light := lightBlock.Find(css)
	if light == nil {
		t.Fatal("no prefers-color-scheme: light block")
	}
	root := rootBlockRe.Find(css)
	if root == nil {
		t.Fatal("no :root block")
	}

	lightVars := map[string]bool{}
	for _, m := range cssVarDecl.FindAllStringSubmatch(string(light), -1) {
		lightVars[m[1]] = true
	}
	for _, m := range cssVarDecl.FindAllStringSubmatch(string(root), -1) {
		name, value := m[1], strings.TrimSpace(m[2])
		if !isColour.MatchString(value) {
			continue // --mono, --sans, --radius: nothing to re-theme
		}
		if !lightVars[name] {
			t.Errorf("%s is a colour in :root with no light value (%s)", name, value)
		}
	}
}

// Without color-scheme the browser draws its own parts — select dropdowns, the
// search field, scrollbars — in light mode against a near-black page.
func TestBothSchemesDeclareColorScheme(t *testing.T) {
	css, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	root := string(rootBlockRe.Find(css))
	if !strings.Contains(root, "color-scheme: dark") {
		t.Error(":root does not declare color-scheme: dark")
	}
	light := string(lightBlock.Find(css))
	if !strings.Contains(light, "color-scheme: light") {
		t.Error("the light block does not declare color-scheme: light")
	}
}
