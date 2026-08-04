// Command shcr is the shellcrumbs binary: shell history capture with a full
// event log behind it.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/shellcrumbs/shcr/internal/config"
	"github.com/shellcrumbs/shcr/internal/crypto"
	"github.com/shellcrumbs/shcr/internal/daemon"
	"github.com/shellcrumbs/shcr/internal/event"
	"github.com/shellcrumbs/shcr/internal/gitinfo"
	"github.com/shellcrumbs/shcr/internal/histfile"
	"github.com/shellcrumbs/shcr/internal/ipc"
	"github.com/shellcrumbs/shcr/internal/paths"
	"github.com/shellcrumbs/shcr/internal/redact"
	"github.com/shellcrumbs/shcr/internal/service"
	"github.com/shellcrumbs/shcr/internal/shell"
	"github.com/shellcrumbs/shcr/internal/store"
	syncengine "github.com/shellcrumbs/shcr/internal/sync"
	"github.com/shellcrumbs/shcr/internal/theme"
	"github.com/shellcrumbs/shcr/internal/tui"
	"github.com/shellcrumbs/shcr/internal/web"
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
	case "nudge":
		// Best effort by design: no daemon means nothing to tell, and the shell
		// must never see an error for it.
		if len(os.Args) > 2 {
			_ = ipc.Nudge(os.Args[2])
		}
	case "init":
		err = cmdInit(os.Args[2:])
	case "tui":
		err = cmdTUI(os.Args[2:])
	case "web":
		err = cmdWeb(os.Args[2:])
	case "service":
		err = cmdService(os.Args[2:])
	case "list", "ls":
		err = cmdList(os.Args[2:])
	case "stats":
		err = cmdStats(os.Args[2:])
	case "key":
		err = cmdKey(os.Args[2:])
	case "sync":
		err = cmdSync(os.Args[2:])
	case "redact":
		err = cmdRedact(os.Args[2:])
	case "import":
		err = cmdImport(os.Args[2:])
	case "export":
		err = cmdExport(os.Args[2:])
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
  tui                    the Ctrl+R picker; prints the chosen command
  web                    serve the local dashboard on 127.0.0.1
  service install|...    run the daemon under systemd
  list                   show recorded commands
  stats                  summarise what has been recorded
  key init|show|import   manage the end-to-end encryption key
  sync now|status|enable cross-machine sync
  redact <id>            replace a recorded command with a tombstone
  import [file...]       bring in an existing shell history file
  export                 write your history out, to stdout or a file
  event start|end        record an event (called by the shell hooks)
  nudge <reason>         tell the daemon a sync is worthwhile (called by the hooks)
  version

getting started:
  eval "$(shcr init bash)"      # add to ~/.bashrc
  shcr daemon &                 # or run under systemd

second machine:
  shcr key show                 # on the first machine, write the words down
  shcr key import               # on the new one
  shcr sync enable --dir <shared-bucket-dir>
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

	// Sync runs alongside capture when it is configured, and is woken by local
	// activity rather than only by its own timer.
	//
	// The loop starts whenever a backend exists, not only when sync is switched
	// on, and asks the configuration again before each cycle. Deciding once at
	// startup meant the dashboard's sync toggle did nothing until the daemon was
	// restarted, with no indication that was what it was waiting for.
	if cfg, err := config.Load(); err != nil {
		logger.Printf("config: %v", err)
	} else if cfg.Sync.Backend != "" {
		// syncEngineWith, not syncEngine: the latter opens a database of its own,
		// which this would then drop on the floor while holding it open for the
		// life of the daemon.
		engine, err := syncEngineWith(st)
		if err != nil {
			logger.Printf("sync disabled: %v", err)
		} else {
			engine.Logger = logger
			engine.Enabled = func() bool {
				c, err := config.Load()
				return err == nil && c.Sync.Enabled
			}
			engine.EnableTriggers()
			d.OnTrigger = func(reason string) { engine.Trigger(syncengine.Trigger(reason)) }
			// Coming up is itself worth a sync: the machine may have been off
			// while other machines were busy.
			engine.Trigger(syncengine.TriggerDaemonStart)
			go func() {
				if err := engine.Run(ctx, syncengine.DefaultLoopConfig()); err != nil && ctx.Err() == nil {
					logger.Printf("sync loop stopped: %v", err)
				}
			}()
			if cfg.Sync.Enabled {
				logger.Printf("sync enabled (%s)", cfg.Sync.Path)
			} else {
				logger.Printf("sync is configured but switched off (%s); it will start when turned on",
					cfg.Sync.Path)
			}
		}
	}

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

func cmdKey(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("key: want 'init', 'show' or 'import'")
	}
	ks := crypto.NewKeystore(paths.DataDir())

	switch args[0] {
	case "init":
		if _, _, err := ks.Load(); err == nil {
			return fmt.Errorf("a key already exists; `shcr key show` prints it, and replacing it would orphan everything already in the bucket")
		}
		k, err := crypto.GenerateKey()
		if err != nil {
			return err
		}
		src, err := ks.Save(k)
		if err != nil {
			return err
		}
		phrase, err := k.Phrase()
		if err != nil {
			return err
		}
		fmt.Printf("A new encryption key was generated and stored in the %s.\n\n", src)
		printPhrase(phrase)
		fmt.Print("\nWrite these words down. They are the only way to read your history on\nanother machine, and nobody else can recover them for you.\n")
		warnIfFile(src)
		return nil

	case "show":
		fs := flag.NewFlagSet("key show", flag.ExitOnError)
		reveal := fs.Bool("reveal", false, "actually print the recovery phrase")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		k, src, err := ks.Load()
		if err != nil {
			return err
		}
		if !*reveal {
			fmt.Printf("A key is configured (stored in the %s).\n", src)
			fmt.Println("Re-run with --reveal to print the recovery phrase.")
			warnIfFile(src)
			return nil
		}
		phrase, err := k.Phrase()
		if err != nil {
			return err
		}
		printPhrase(phrase)
		warnIfFile(src)
		return nil

	case "import":
		if _, _, err := ks.Load(); err == nil {
			return fmt.Errorf("a key already exists on this machine; remove it first if you really mean to replace it")
		}
		fmt.Print("Paste the 24-word recovery phrase from your other machine:\n> ")
		line, err := readSecretLine()
		if err != nil && line == "" {
			return err
		}
		k, err := crypto.KeyFromPhrase(line)
		if err != nil {
			return err
		}
		src, err := ks.Save(k)
		if err != nil {
			return err
		}
		fmt.Printf("Key accepted and stored in the %s.\n", src)
		warnIfFile(src)
		return nil
	}
	return fmt.Errorf("key: unknown subcommand %q", args[0])
}

// readSecretLine reads one line, without echoing it when the input is a
// terminal. `key show` puts the same words behind --reveal; echoing them on the
// way in leaves the key to the whole history in the terminal's scrollback, and
// often in a session log or a screen share as well.
func readSecretLine() (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		// Piped or redirected input is not echoed by anything in the first place.
		return bufio.NewReader(os.Stdin).ReadString('\n')
	}
	b, err := term.ReadPassword(fd)
	// ReadPassword consumes the newline without printing it, so the next line of
	// output would otherwise run on from the prompt.
	fmt.Println()
	return string(b), err
}

func printPhrase(phrase string) {
	words := strings.Fields(phrase)
	for i, w := range words {
		fmt.Printf("%2d. %-10s", i+1, w)
		if (i+1)%4 == 0 {
			fmt.Println()
		}
	}
	if len(words)%4 != 0 {
		fmt.Println()
	}
}

func warnIfFile(src crypto.Source) {
	if src == crypto.SourceFile {
		fmt.Fprintf(os.Stderr,
			"\nwarning: no OS keychain was available, so the key is in a 0600 file at\n"+
				"         %s\n"+
				"         Anyone who can read that file can read your synced history.\n",
			crypto.NewKeystore(paths.DataDir()).FilePath)
	}
}

// ---------------------------------------------------------------- sync

func syncEngine() (*syncengine.Engine, error) {
	st, err := openStore()
	if err != nil {
		return nil, err
	}
	e, err := syncEngineWith(st)
	if err != nil {
		st.Close()
		return nil, err
	}
	return e, nil
}

// syncEngineWith builds the engine around an already-open database, so the
// daemon and the web server do not each hold their own handle.
func syncEngineWith(st *store.Store) (*syncengine.Engine, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if cfg.Sync.Backend == "" {
		return nil, fmt.Errorf("sync is not configured (run: shcr sync enable --dir <path>)")
	}
	if cfg.Sync.Backend != "file" {
		return nil, fmt.Errorf("unknown sync backend %q", cfg.Sync.Backend)
	}
	storage, err := syncengine.NewFileStorage(cfg.Sync.Path)
	if err != nil {
		return nil, err
	}
	ks := crypto.NewKeystore(paths.DataDir())
	deviceID, err := paths.DeviceID()
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	return &syncengine.Engine{
		Store: st, Storage: storage,
		// Resolved on first use, not here: a daemon under systemd starts before
		// the keyring is unlocked.
		KeyFunc:  func() (crypto.Key, error) { k, _, err := ks.Load(); return k, err },
		DeviceID: deviceID, Hostname: host,
		ShareHostname: cfg.Sync.ShareHostname,
		Logger:        log.New(os.Stderr, "shcr: ", 0),
	}, nil
}

func cmdSync(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("sync: want 'now', 'status' or 'enable'")
	}
	switch args[0] {
	case "enable":
		fs := flag.NewFlagSet("sync enable", flag.ExitOnError)
		dir := fs.String("dir", "", "directory to use as the bucket")
		shareHost := fs.Bool("share-hostname", false, "put a hostname hint in the manifest (visible to the storage provider)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *dir == "" {
			return fmt.Errorf("sync enable: --dir is required")
		}
		abs, err := filepath.Abs(*dir)
		if err != nil {
			return err
		}
		ks := crypto.NewKeystore(paths.DataDir())
		if _, _, err := ks.Load(); err != nil {
			return fmt.Errorf("no encryption key yet: run `shcr key init` (or `shcr key import` on a second machine) first")
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		cfg.Sync = config.Sync{Enabled: true, Backend: "file", Path: abs, ShareHostname: *shareHost}
		if err := config.Save(cfg); err != nil {
			return err
		}
		fmt.Printf("Sync enabled, using %s\n", abs)
		fmt.Println("Restart the daemon to pick this up, or run `shcr sync now`.")
		return nil

	case "now":
		e, err := syncEngine()
		if err != nil {
			return err
		}
		defer e.Store.Close()
		pushed, pulled, err := e.SyncOnce(context.Background())
		if err != nil {
			return err
		}
		fmt.Printf("pushed %d event(s), pulled %d event(s)\n", pushed, pulled)
		return nil

	case "status":
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		st, err := openStore()
		if err != nil {
			return err
		}
		defer st.Close()
		deviceID, _ := paths.DeviceID()

		th := theme.New(os.Stdout)
		pending, err := st.CountUnsynced(deviceID)
		if err != nil {
			return err
		}
		backend := "not configured"
		if cfg.Sync.Backend != "" {
			backend = fmt.Sprintf("%s backend at %s", cfg.Sync.Backend, cfg.Sync.Path)
		}
		for _, row := range [][2]string{
			{"sync", backend},
			{"device", deviceID + " (this machine)"},
			{"pending", fmt.Sprintf("%d event(s) waiting to upload", pending)},
		} {
			fmt.Printf("%s  %s\n", th.Label.Render(fmt.Sprintf("%-8s", row[0])), row[1])
		}

		cursors, err := st.Cursors()
		if err != nil {
			return err
		}
		if len(cursors) == 0 {
			fmt.Printf("%s  %s\n", th.Label.Render(fmt.Sprintf("%-8s", "peers")), th.Muted.Render("none seen yet"))
			return nil
		}
		fmt.Println(th.Label.Render(fmt.Sprintf("%-8s", "peers")))
		for _, c := range cursors {
			name := c.HostnameHint
			if name == "" {
				name = "(unnamed)"
			}
			fmt.Printf("  %-38s %-16s %s\n", c.PeerDeviceID, name,
				th.Muted.Render("last synced "+time.UnixMilli(c.LastSyncedAt).Format("2006-01-02 15:04")))
		}
		return nil
	}
	return fmt.Errorf("sync: unknown subcommand %q", args[0])
}

// ---------------------------------------------------------------- import

func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "report what would be imported without writing anything")
	if err := fs.Parse(args); err != nil {
		return err
	}
	th := theme.New(os.Stdout)

	files := fs.Args()
	if len(files) == 0 {
		files = histfile.Discover()
		if len(files) == 0 {
			return fmt.Errorf("found no history files; pass one explicitly")
		}
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
	host, _ := os.Hostname()
	red := redactor()

	var totalNew, totalSeen, totalRedacted, totalSkipped int
	for _, p := range files {
		src, err := histfile.Parse(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "shcr: %s: %v\n", p, err)
			continue
		}

		var added, existing, redacted, skipped int
		for _, e := range src.Entries {
			text, action, _ := red.Apply(e.Command)
			if action == redact.ActionSkip {
				skipped++
				continue
			}
			if action == redact.ActionRedact {
				redacted++
			}
			ev := importEvent(deviceID, host, string(src.Kind), src.Path, text, e)
			if *dryRun {
				// An entry's identity is derived from its content, so whether it is
				// already here is knowable without writing anything. Counting every
				// entry as new made the one number this flag exists to produce wrong
				// for the common case of re-importing a file.
				c, err := st.CommandByID(ev.CommandID)
				if err != nil {
					return fmt.Errorf("%s: %w", p, err)
				}
				if c == nil {
					added++
				} else {
					existing++
				}
				continue
			}
			inserted, err := st.AppendEvent(ev)
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			if inserted {
				added++
			} else {
				existing++
			}
		}

		fmt.Printf("%s %s\n", th.Label.Render(fmt.Sprintf("%-6s", string(src.Kind))), p)
		detail := fmt.Sprintf("%d new", added)
		if existing > 0 {
			detail += fmt.Sprintf(", %d already present", existing)
		}
		if redacted > 0 {
			detail += fmt.Sprintf(", %d with secrets redacted", redacted)
		}
		if skipped > 0 {
			detail += fmt.Sprintf(", %d skipped entirely", skipped)
		}
		fmt.Printf("       %s\n", th.Muted.Render(detail))

		totalNew += added
		totalSeen += existing
		totalRedacted += redacted
		totalSkipped += skipped
	}

	fmt.Println()
	if *dryRun {
		msg := fmt.Sprintf("dry run: %d command(s) would be imported", totalNew)
		if totalSeen > 0 {
			msg += fmt.Sprintf("; %d already here", totalSeen)
		}
		fmt.Println(th.Muted.Render(msg))
		return nil
	}
	fmt.Printf("imported %d command(s)", totalNew)
	if totalSeen > 0 {
		fmt.Printf("; %d were already here", totalSeen)
	}
	fmt.Println()
	fmt.Println(th.Muted.Render(
		"Imported commands carry no exit code, and a time only where the shell recorded one."))
	return nil
}

// importEvent builds an event whose identity is derived from its content, so
// importing the same file twice adds nothing the second time. A history file is
// re-read, appended to and trimmed constantly; anything keyed on position would
// duplicate every entry the first time the file rolled over.
func importEvent(deviceID, host, shell, source, text string, e histfile.Entry) event.Event {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"shcr-import-v1", shell, strconv.FormatInt(e.StartTime, 10), text,
	}, "\x00")))
	id := "import-" + hex.EncodeToString(sum[:12])

	payload, _ := json.Marshal(event.ImportPayload{
		Command:         text,
		Hostname:        host,
		Shell:           shell,
		StartTime:       e.StartTime,
		ApproximateTime: e.Approximate,
		DurationMS:      e.DurationMS,
		Source:          filepath.Base(source),
	})
	return event.Event{
		EventID:   id,
		CommandID: id,
		DeviceID:  deviceID,
		Type:      event.TypeImport,
		Payload:   payload,
		CreatedAt: e.StartTime,
	}
}

// ---------------------------------------------------------------- export

func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	format := fs.String("format", "jsonl", "jsonl, json or csv")
	events := fs.Bool("events", false, "export the raw event log instead of commands (lossless)")
	out := fs.String("o", "", "write to this file instead of stdout (created 0600)")
	query := fs.String("q", "", "full-text search over the command")
	status := fs.String("status", "", "running|completed|failed|orphaned")
	host := fs.String("host", "", "filter by hostname")
	session := fs.String("session", "", "filter by session id")
	cwd := fs.String("cwd", "", "filter by working directory")
	since := fs.Duration("since", 0, "only commands newer than this (e.g. 720h)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	w := io.Writer(os.Stdout)
	if *out != "" {
		// This file is the whole history in plain text, so it gets the same
		// permissions as the database it came from.
		//
		// O_NOFOLLOW because writing through a symlink puts the history wherever
		// whoever created that link chose, which on a shared machine with a
		// predictable path is somebody else's decision to make.
		f, err := os.OpenFile(*out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			if errors.Is(err, syscall.ELOOP) {
				return fmt.Errorf("%s is a symlink; refusing to write your history through it", *out)
			}
			return err
		}
		defer f.Close()
		// The mode in OpenFile applies only when it creates the file. Exporting
		// over one that already existed — the same filename twice, or a file
		// something else made — otherwise left the whole history at whatever
		// permissions it already had.
		if err := f.Chmod(0o600); err != nil {
			return fmt.Errorf("securing %s: %w", *out, err)
		}
		w = f
	}
	buf := bufio.NewWriterSize(w, 128*1024)
	defer buf.Flush()

	f := store.Filter{
		Text: *query, Status: *status, Hostname: *host,
		SessionID: *session, Cwd: *cwd,
	}
	if *since > 0 {
		f.Since = time.Now().Add(-*since).UnixMilli()
	}

	var n int
	switch strings.ToLower(*format) {
	case "jsonl":
		enc := json.NewEncoder(buf)
		if *events {
			err = st.EachEvent(func(ev event.Event) error { n++; return enc.Encode(ev) })
		} else {
			err = st.EachCommand(f, func(c store.Command) error { n++; return enc.Encode(c) })
		}
	case "json":
		// One array, streamed element by element rather than assembled in memory.
		if _, err := buf.WriteString("[\n"); err != nil {
			return err
		}
		enc := json.NewEncoder(buf)
		emit := func(v any) error {
			if n > 0 {
				if _, err := buf.WriteString(","); err != nil {
					return err
				}
			}
			n++
			return enc.Encode(v)
		}
		if *events {
			err = st.EachEvent(func(ev event.Event) error { return emit(ev) })
		} else {
			err = st.EachCommand(f, func(c store.Command) error { return emit(c) })
		}
		if err == nil {
			_, err = buf.WriteString("]\n")
		}
	case "csv":
		if *events {
			return fmt.Errorf("the event log has a nested payload; use --format jsonl for --events")
		}
		cw := csv.NewWriter(buf)
		defer cw.Flush()
		if err := cw.Write([]string{
			"id", "started", "command", "host", "cwd", "branch", "shell",
			"status", "exit_code", "duration_ms", "session", "imported",
		}); err != nil {
			return err
		}
		err = st.EachCommand(f, func(c store.Command) error {
			n++
			return cw.Write([]string{
				c.ID,
				time.UnixMilli(c.StartTime).Format(time.RFC3339),
				c.Command, c.Hostname, c.Cwd, derefString(c.GitBranch), c.Shell,
				c.Status, derefInt(c.ExitCode), derefInt64(c.DurationMS),
				c.SessionID, strconv.FormatBool(c.Imported),
			})
		})
	default:
		return fmt.Errorf("unknown format %q (want jsonl, json or csv)", *format)
	}
	if err != nil {
		return err
	}
	if err := buf.Flush(); err != nil {
		return err
	}

	// The count goes to stderr so stdout stays a clean stream to pipe.
	kind := "command"
	if *events {
		kind = "event"
	}
	fmt.Fprintf(os.Stderr, "exported %d %s(s)", n, kind)
	if *out != "" {
		fmt.Fprintf(os.Stderr, " to %s", *out)
	}
	fmt.Fprintln(os.Stderr)
	if !*events {
		fmt.Fprintln(os.Stderr,
			"note: this is the derived view. `--events` exports the log it is derived from,")
		fmt.Fprintln(os.Stderr,
			"      which is lossless and the only form that can rebuild this database.")
	}
	return nil
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}

func derefInt64(p *int64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatInt(*p, 10)
}

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

func cmdTUI(args []string) error {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	query := fs.String("query", "", "text already typed at the prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	// Catch up the ranking cache before drawing. The daemon keeps it warm, so
	// this is normally the handful of commands run since its last pass and
	// costs well under a millisecond — but the picker must not be the one place
	// that cannot see what you just ran, and it has to work with no daemon at
	// all. A failure here is not worth refusing to open over: stale ranking is
	// a worse list, not a broken one.
	if from, err := st.StatsWatermark(); err == nil {
		if _, _, err := st.RefreshCommandStats(from); err != nil {
			fmt.Fprintf(os.Stderr, "shcr: ranking cache: %v\n", err)
		}
	}

	chosen, err := tui.Run(st, *query)
	if err != nil {
		return err
	}
	// stdout carries the selection and nothing else — the shell captures it and
	// puts it in the prompt. Printing it is the whole contract; running it is
	// deliberately not our decision to make.
	if chosen != "" {
		fmt.Print(chosen)
	}
	return nil
}

// ---------------------------------------------------------------- service

func cmdService(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("service: want 'install', 'uninstall', 'status' or 'units'")
	}
	th := theme.New(os.Stdout)

	switch args[0] {
	case "units":
		// Print without installing, so the units can be inspected or managed by
		// something other than this command.
		binary, err := service.BinaryPath()
		if err != nil {
			return err
		}
		cfg, _ := config.Load()
		socket, svc, err := service.Render(binary, needsNetwork(cfg.Sync.Backend))
		if err != nil {
			return err
		}
		fmt.Printf("%s\n%s\n%s\n%s",
			th.Label.Render("# "+service.SocketUnit), socket,
			th.Label.Render("# "+service.ServiceUnit), svc)
		return nil

	case "install":
		fs := flag.NewFlagSet("service install", flag.ExitOnError)
		start := fs.Bool("start", true, "enable and start the units immediately")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if err := service.Available(); err != nil {
			return err
		}
		binary, err := service.BinaryPath()
		if err != nil {
			return err
		}
		cfg, _ := config.Load()
		written, err := service.Install(binary, needsNetwork(cfg.Sync.Backend))
		if err != nil {
			return err
		}
		for _, p := range written {
			fmt.Printf("%s %s\n", th.Label.Render("wrote"), p)
		}
		if err := service.Systemctl("daemon-reload"); err != nil {
			return err
		}
		if *start {
			// Enabling the socket rather than the service is the point: systemd
			// creates the socket at boot and starts the daemon on first use.
			if err := service.Systemctl("enable", "--now", service.SocketUnit); err != nil {
				return err
			}
			if err := service.Systemctl("enable", "--now", service.ServiceUnit); err != nil {
				return err
			}
			fmt.Println(th.Label.Render("started") + " shcr.socket and shcr.service")
		}
		if service.InTree(binary) {
			fmt.Fprintf(os.Stderr, "\nnote: the unit points at %s, which looks like a build directory.\n"+
				"      Rebuilding there will replace the binary underneath the running service.\n"+
				"      Consider copying it to ~/.local/bin and re-running this.\n", binary)
		}
		if hint := service.LingerHint(os.Getenv("USER")); hint != "" {
			fmt.Fprintf(os.Stderr, "\n%s\n", hint)
		}
		return nil

	case "uninstall":
		removed, err := service.Uninstall()
		if err != nil {
			return err
		}
		if len(removed) == 0 {
			fmt.Println("no units were installed")
		}
		for _, p := range removed {
			fmt.Printf("%s %s\n", th.Label.Render("removed"), p)
		}
		fmt.Println()
		fmt.Println(th.Muted.Render("Your history was left alone. To remove it as well:"))
		for _, p := range service.DataPaths() {
			fmt.Println(th.Muted.Render("    rm -rf " + p))
		}
		return nil

	case "status":
		if err := service.Available(); err != nil {
			return err
		}
		return service.Systemctl("status", "--no-pager", service.SocketUnit, service.ServiceUnit)
	}
	return fmt.Errorf("service: unknown subcommand %q", args[0])
}

// needsNetwork reports whether the configured sync backend talks to the
// network, which decides whether the unit may open internet sockets at all.
func needsNetwork(backend string) bool {
	return backend != "" && backend != "file"
}

// ---------------------------------------------------------------- web

func cmdWeb(args []string) error {
	fs := flag.NewFlagSet("web", flag.ExitOnError)
	port := fs.Int("port", 0, "port to bind (0 picks a free one)")
	open := fs.Bool("open", false,
		"open the dashboard in a browser (passes the token to the browser launcher, "+
			"where it is visible in the process list — avoid on a shared machine)")
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
	host, _ := os.Hostname()
	logger := log.New(os.Stderr, "shcr: ", log.LstdFlags)

	srv, err := web.New(st, deviceID, host, logger)
	if err != nil {
		return err
	}

	// The engine is built per request rather than once at startup. The
	// dashboard can turn sync on and off, and an engine bound here would go on
	// answering for whatever the configuration said when the page was first
	// served — so switching sync on and pressing Sync did nothing at all.
	srv.Sync = func(ctx context.Context) (int, int, error) {
		cfg, err := config.Load()
		if err != nil {
			return 0, 0, err
		}
		if !cfg.Sync.Enabled {
			return 0, 0, fmt.Errorf("sync is turned off")
		}
		engine, err := syncEngineWith(st)
		if err != nil {
			return 0, 0, err
		}
		return engine.SyncOnce(ctx)
	}

	ln, err := srv.Listen(*port)
	if err != nil {
		return err
	}
	url := srv.URL(ln)
	fmt.Println(url)
	fmt.Fprintln(os.Stderr, "The token in that URL is required; it changes every time the server starts.")
	if *open {
		web.OpenBrowser(url)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return srv.Serve(ctx, ln)
}

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
		// So a command still running shows how long it has been going.
		Now: time.Now().UnixMilli(),
	}
	// One row from elsewhere reserves the host column on all of them, so the
	// command text ends at the same column the whole way down.
	for _, c := range cmds {
		if c.Hostname != "" && c.Hostname != localHost {
			opts.ReserveHost = true
			break
		}
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
				fmt.Println(th.Muted.Render("                 " + theme.Safe(line)))
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
