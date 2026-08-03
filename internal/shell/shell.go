// Package shell holds the init snippets that `shcr init <shell>` emits.
package shell

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed bash.sh
var bashScript string

//go:embed zsh.zsh
var zshScript string

//go:embed fish.fish
var fishScript string

// Supported lists the shells we ship hooks for.
var Supported = []string{"bash", "zsh", "fish"}

// InitScript returns the integration snippet for a shell, with the absolute
// path of the running binary baked in so the hooks do not depend on $PATH.
func InitScript(name string) (string, error) {
	var script string
	switch name {
	case "bash":
		script = bashScript
	case "zsh":
		script = zshScript
	case "fish":
		script = fishScript
	default:
		return "", fmt.Errorf("unsupported shell %q (want one of %s)", name, strings.Join(Supported, ", "))
	}
	return strings.ReplaceAll(script, "@SHCR_BIN@", binaryPath()), nil
}

func binaryPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "shcr"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved
	}
	return exe
}
