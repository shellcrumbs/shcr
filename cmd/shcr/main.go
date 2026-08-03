// Command shcr is the shellcrumbs binary: shell history capture with a full
// event log behind it.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/shellcrumbs/shcr/internal/config"
	"github.com/shellcrumbs/shcr/internal/daemon"
	"github.com/shellcrumbs/shcr/internal/event"
	"github.com/shellcrumbs/shcr/internal/gitinfo"
	"github.com/shellcrumbs/shcr/internal/ipc"
	"github.com/shellcrumbs/shcr/internal/paths"
	"github.com/shellcrumbs/shcr/internal/redact"
	"github.com/shellcrumbs/shcr/internal/shell"
	"github.com/shellcrumbs/shcr/internal/store"
	"github.com/shellcrumbs/shcr/internal/theme"
)

// commandEnvVar carries command text from the shell hook to `shcr event`
// without it appearing in the world-readable process argument list.
const commandEnvVar = "SHCR_COMMAND"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "daemon":
		err = cmdDaemon(os.Args[2:])
	case "event":
		err = cmdEvent(os.Args[2:])
	case "init":
		err = cmdInit(os.Args[2:])
	case "list", "ls":
		err = cmdList(os.Args[2:])
	case "stats":
		err = cmdStats(os.Args[2:])
	case "redact":
		err = cmdRedact(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(versionString())
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "shcr: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		th := theme.New(os.Stderr)
		fmt.Fprintln(os.Stderr, th.Error.Render("shcr:")+" "+err.Error())
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `shellcrumbs — shell history that knows what is still running

usage: shcr <command> [flags]

  init <bash|zsh|fish>   print the shell integration snippet
  daemon                 run the capture daemon
  list                   show recorded commands
  stats                  summarise what has been recorded
  redact <id>            replace a recorded command with a tombstone
  event start|end        record an event (called by the shell hooks)
  version

getting started:
  eval "$(shcr init bash)"      # add to ~/.bashrc
  shcr daemon &                 # or run under systemd
`)
}

func openStore() (*store.Store, error) {
	if err := paths.EnsureDirs(); err != nil {
		return nil, err
	}
	return store.Open(paths.DBPath())
}

// ---------------------------------------------------------------- daemon

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	quiet := fs.Bool("quiet", false, "suppress log output")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	deviceID, err := paths.DeviceID()
	if err != nil {
		return err
	}

	logger := log.New(os.Stderr, "shcr: ", log.LstdFlags)
	if *quiet {
		// io.Discard, not os.NewFile(0, os.DevNull): NewFile wraps the numbered
		// descriptor and takes the name only as a label, so that opened fd 0 and
		// --quiet wrote the log to stdin — the terminal, when stdin is one.
		logger = log.New(io.Discard, "", 0)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	d := daemon.New(st, deviceID, logger, redactor())

	return d.Run(ctx)
}

// ---------------------------------------------------------------- event

func cmdEvent(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("event: want 'start' or 'end'")
	}
	deviceID, err := paths.DeviceID()
	if err != nil {
		return err
	}

	switch args[0] {
	case "start":
		fs := flag.NewFlagSet("event start", flag.ExitOnError)
		id := fs.String("id", "", "command id")
		command := fs.String("command", "", "command text")
		session := fs.String("session", "", "shell session id")
		cwd := fs.String("cwd", "", "working directory")
		sh := fs.String("shell", "", "shell name")
		pgid := fs.Int("pgid", 0, "process group id of the shell")
		ts := fs.Int64("time", 0, "start time in unix millis")
		background := fs.Bool("background", false, "command was backgrounded with &")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		// The command text comes through the environment by default. /proc/<pid>/
		// cmdline is world-readable on Linux while /proc/<pid>/environ is not, so
		// passing it as an argument would publish every recorded command to every
		// other account on the machine for the few milliseconds this process
		// lives — including shell builtins like `export TOKEN=...`, which never
		// appear in the process list at all on their own. The flag is kept for
		// running this by hand.
		raw := *command
		if raw == "" {
			raw = os.Getenv(commandEnvVar)
		}
		if *id == "" || raw == "" {
			return fmt.Errorf("event start: --id and a command (via %s or --command) are required", commandEnvVar)
		}
		text, action, _ := redactor().Apply(raw)
		if action == redact.ActionSkip {
			// Nothing is recorded at all, and nothing is written anywhere.
			return nil
		}

		host, _ := os.Hostname()
		when := *ts
		if when == 0 {
			when = event.NowMillis()
		}
		p := event.StartPayload{
			Command:      text,
			Hostname:     host,
			SessionID:    *session,
			Cwd:          *cwd,
			GitBranch:    gitinfo.Branch(*cwd),
			Shell:        *sh,
			StartTime:    when,
			PGID:         *pgid,
			IsBackground: *background,
		}
		ev, err := event.New(*id, deviceID, event.TypeStart, p)
		if err != nil {
			return err
		}
		_, err = ipc.Send(ev)
		return err

	case "end":
		fs := flag.NewFlagSet("event end", flag.ExitOnError)
		id := fs.String("id", "", "command id")
		exit := fs.Int("exit", 0, "exit code")
		ts := fs.Int64("time", 0, "end time in unix millis")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *id == "" {
			return fmt.Errorf("event end: --id is required")
		}
		when := *ts
		if when == 0 {
			when = event.NowMillis()
		}
		ev, err := event.New(*id, deviceID, event.TypeEnd, event.EndPayload{EndTime: when, ExitCode: *exit})
		if err != nil {
			return err
		}
		_, err = ipc.Send(ev)
		return err
	}
	return fmt.Errorf("event: unknown kind %q", args[0])
}

// redactor builds the secret filter from the built-in rules plus the user's.
// A broken user config is reported and then ignored rather than aborting the
// capture: losing the extra rules is bad, losing the command is worse, and the
// built-ins still apply.
func redactor() *redact.Redactor {
	rules, err := redact.LoadRules(config.RedactConfigPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "shcr: ignoring %s: %v\n", config.RedactConfigPath(), err)
	}
	return redact.New(rules)
}

// ---------------------------------------------------------------- key

// ---------------------------------------------------------------- sync

// ---------------------------------------------------------------- import

// ---------------------------------------------------------------- export

// ---------------------------------------------------------------- redact

func cmdRedact(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("redact: want exactly one command id (see `shcr list`)")
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	c, err := st.CommandByID(args[0])
	if err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("no command with id %q", args[0])
	}
	deviceID, err := paths.DeviceID()
	if err != nil {
		return err
	}
	// A redaction is an event like any other, which is what lets it travel to
	// every other machine. Redacting only locally, while the secret sits in each
	// peer's copy, would be worse than doing nothing.
	ev, err := event.New(c.ID, deviceID, event.TypeRedact, map[string]any{
		"reason":      "manual",
		"redacted_at": event.NowMillis(),
	})
	if err != nil {
		return err
	}
	if _, err := st.AppendEvent(ev); err != nil {
		return err
	}
	fmt.Printf("redacted %s\n", c.ID)
	fmt.Println("The replacement will reach your other machines on the next sync.")
	return nil
}

// ---------------------------------------------------------------- init

func cmdInit(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("init: want one of %s", strings.Join(shell.Supported, ", "))
	}
	s, err := shell.InitScript(args[0])
	if err != nil {
		return err
	}
	fmt.Print(s)
	return nil
}

// ---------------------------------------------------------------- tui

// ---------------------------------------------------------------- service

// ---------------------------------------------------------------- web

// ---------------------------------------------------------------- list

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	limit := fs.Int("n", 25, "how many to show")
	query := fs.String("q", "", "full-text search over the command")
	status := fs.String("status", "", "running|completed|failed|orphaned")
	host := fs.String("host", "", "filter by hostname")
	session := fs.String("session", "", "filter by session id")
	cwd := fs.String("cwd", "", "filter by working directory")
	since := fs.Duration("since", 0, "only commands newer than this (e.g. 2h)")
	full := fs.Bool("full", false, "print full multi-line commands instead of one line each")
	colorMode := fs.String("color", "auto", "auto, always or never")
	if err := fs.Parse(args); err != nil {
		return err
	}
	mode, err := theme.ParseColorMode(*colorMode)
	if err != nil {
		return err
	}

	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	f := store.Filter{
		Text:      *query,
		Hostname:  *host,
		Status:    *status,
		SessionID: *session,
		Cwd:       *cwd,
		Limit:     *limit,
	}
	if *since > 0 {
		f.Since = time.Now().Add(-*since).UnixMilli()
	}
	cmds, err := st.QueryCommands(f)
	if err != nil {
		return err
	}
	th := theme.NewWithMode(os.Stdout, mode)
	if len(cmds) == 0 {
		// "nothing recorded" and "nothing matched" are different facts, and
		// saying the first when the second is true sends people looking for a
		// broken daemon.
		filtered := *query != "" || *status != "" || *host != "" || *session != "" || *cwd != "" || *since > 0
		if filtered {
			fmt.Println(th.Muted.Render("no commands match those filters"))
		} else {
			fmt.Println(th.Muted.Render("no commands recorded yet — is the daemon running?"))
		}
		return nil
	}
	localHost, _ := os.Hostname()
	baseCwd, _ := os.Getwd()
	opts := theme.RowOpts{
		Width:     terminalWidth(),
		ShowTime:  true,
		Tokens:    theme.Tokens(*query),
		LocalHost: localHost,
		// Showing the directory only when it differs from where the user is
		// standing keeps the common case — listing history from the project you
		// are working in — free of a repeated path on every row.
		BaseCwd: baseCwd,
	}

	// Oldest first reads like a terminal scrollback.
	//
	// Rows carry a clock time only, which is ambiguous the moment a listing
	// spans midnight. Rather than widen every row with a date, the day is
	// announced once when it changes — and not at all when everything is from
	// today, which is the common case.
	today := time.Now().Format("2006-01-02")
	lastDay := today
	for i := len(cmds) - 1; i >= 0; i-- {
		c := cmds[i]
		if day := time.UnixMilli(c.StartTime).Format("2006-01-02"); day != lastDay {
			fmt.Println(th.Muted.Render("  ── " + time.UnixMilli(c.StartTime).Format("Mon 2 Jan 2006")))
			lastDay = day
		}
		fmt.Println(strings.TrimRight(th.Row(c, opts), " "))
		// The row already shows the first line and marks it as continued, so
		// --full only has to supply what was cut off.
		if *full && strings.Contains(c.Command, "\n") {
			for _, line := range strings.Split(c.Command, "\n")[1:] {
				fmt.Println(th.Muted.Render("                 " + line))
			}
		}
	}
	return nil
}

// terminalWidth reports the usable width, falling back to a sane fixed value
// when output is redirected — a piped listing should not depend on whatever
// terminal happens to be attached.
func terminalWidth() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		return min(w, 160)
	}
	return 100
}

// ---------------------------------------------------------------- stats

func cmdStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	total, err := st.CountCommands()
	if err != nil {
		return err
	}
	deviceID, _ := paths.DeviceID()

	th := theme.New(os.Stdout)
	for _, row := range [][2]string{
		{"device", deviceID},
		{"database", paths.DBPath()},
		{"socket", paths.SocketPath()},
		{"commands", fmt.Sprintf("%d", total)},
	} {
		fmt.Printf("%s  %s\n", th.Label.Render(fmt.Sprintf("%-8s", row[0])), row[1])
	}
	// One GROUP BY, rather than loading every row of each status into memory to
	// take its length: on a large history that was four queries of up to a
	// million rows each to print four small numbers.
	stats, err := st.Stats(time.Now())
	if err != nil {
		return err
	}
	for _, row := range []struct {
		status string
		count  int64
	}{
		{store.StatusRunning, stats.Running},
		{store.StatusCompleted, stats.Completed},
		{store.StatusFailed, stats.Failed},
		{store.StatusOrphaned, stats.Orphaned},
	} {
		fmt.Printf("  %s %s %d\n",
			th.Dot(row.status), th.Muted.Render(fmt.Sprintf("%-9s", row.status)), row.count)
	}
	return nil
}
