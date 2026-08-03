# Contributing to Shellcrumbs

```sh
git clone https://github.com/shellcrumbs/shcr && cd shcr
go build -o bin/shcr ./cmd/shcr
go test ./...
```

## Requirements

Go 1.25 or newer.


## How it works

```
  shell hooks ──unix socket──> daemon ──> SQLite ──> picker / list / dashboard
                                 │
                                 └── encrypt ──> shared directory ──> other machines
```

## Code layout

```
cmd/shcr           CLI entry point
internal/event     the append-only event types
internal/store     SQLite: schema, event log, derived rows, search
internal/daemon    socket listener, spool drain, orphan sweep, socket activation
internal/ipc       hook-side sender with spool fallback
internal/shell     embedded bash/zsh/fish integrations
internal/histfile  parsers for existing shell history files
internal/gitinfo   branch lookup by reading .git, no git fork
internal/theme     shared palette, row renderer and formatters
internal/tui       the Ctrl+R picker
internal/web       dashboard API, event stream, embedded assets
internal/crypto    key generation, recovery phrase, batch sealing, keystore
internal/redact    secret patterns, applied before anything is written
internal/sync      storage interface, batching, manifests, cursors, the loop
internal/service   systemd unit generation and install
internal/config    on-disk settings
```

## Invariants

These are the things a reasonable-looking change quietly breaks. Most are held
up by a test that will fail; the ones that are not are noted.

**Command rows are recomputed from all of a command's events, never patched.**
`AppendEvent` rebuilds the row from the whole log for that command id. This is
what makes replaying an event a no-op and makes any arrival order converge —
which matters because sync delivers `end` before `start` sooner or later.
Applying deltas would be faster and wrong.

**A device writes only under its own prefix in the bucket.** No locking, no
conflict resolution and no last-writer-wins exist anywhere in sync because of
this one property. It also means events pulled from a peer must never be
re-uploaded: `InsertRemoteEvent` marks them already-synced, and the upload queue
filters on device id behind that. Lose either and every machine starts echoing
every other machine's history back into the bucket.

**Batch keys are monotonic per device.** A reader tracks a peer as a single
"highest key seen" and skips anything at or below it, so a key that sorts
backwards is not merely out of order — it is never read again. Wall clocks do
step backwards, and two pushes can land in the same millisecond, so the key is
derived from the previous one whenever the clock does not advance past it. Note
the comparison happens at the precision the key is *written* with; comparing a
nanosecond `time.Now()` against a millisecond-truncated key silently does
nothing.

**Listings are bounded by the cursor.** `Storage.List` takes an exclusive
`after`, and peers are found with `Children` rather than by enumerating every
object to read device ids out of paths. Without both, a sync in year five costs
proportionally more than one in week one.

**Orphaned means "no result can ever arrive", not "nothing is running".** The
sweep checks whether the *shell* still exists, not its process group — kill a
terminal during `sleep 100` and the reparented sleep keeps the group alive, so a
group check leaves the command at `running` forever. Related: do not add
`PrivateUsers` to the systemd unit. It makes `kill(pid, 0)` return `EPERM`
against the user's own shells, `EPERM` reads as *still alive*, and orphan
detection stops working with no error anywhere.

**Redaction runs in the sender, not only in the daemon.** The daemon is where
the guarantee is stated, but hooks spool to disk when it is down, so
daemon-only redaction writes plaintext secrets to a file whenever the daemon
happens to be restarting. Both layers apply it; it is idempotent.

**Command text never goes through the argument list.** `/proc/<pid>/cmdline` is
world-readable and `/proc/<pid>/environ` is not, so the hooks pass command text
through the environment. This matters most for shell builtins — `export
TOKEN=...` appears in no process list on its own, so recording it as an argument
would create an exposure that did not otherwise exist.

**Hooks never block the shell, and never overwrite what is already installed.**
They write to a socket and return. `PROMPT_COMMAND`, the `DEBUG` trap, the
`EXIT` trap and zsh's hook arrays are all chained onto, never replaced —
breaking someone's starship or direnv setup is the classic failure of tools in
this category. The picker prints the chosen command to stdout and never runs it.

**Styles come from a renderer bound to the writer being drawn on.** Lipgloss's
package-level default profiles `os.Stdout`; under Ctrl+R that is the shell's
`$(...)` capture pipe, so the default renderer reports a colourless terminal and
discards every style. `theme.New` takes the writer. Relatedly, the terminal
background is set explicitly rather than detected — detection costs an OSC query
with a five second timeout, fired after Bubble Tea has taken the terminal.

**Text is measured in columns, not runes.** A CJK glyph or emoji is two cells
wide. `theme.Truncate`, `TailPath` and `Wrap` are grapheme- and width-aware; rune
counting lets a row render wider than its pane and pushes the picker's border off
screen.

**`StartLimit*` belongs in `[Unit]`, not `[Service]`.** In the wrong section
systemd ignores it with a journal warning nobody reads, and a database the daemon
cannot open restarts forever. `systemd-analyze verify` catches this and is part
of the test suite.

## Testing

```sh
go test ./...            # the full suite
go test -race ./...      # clean; timing assertions skip themselves under it
go vet ./... && gofmt -l .
shellcheck -s bash internal/shell/bash.sh
```

Three parts of this project cannot be tested from Go alone, and each has a way
of looking correct while being wrong.

**Shell hooks need real shells.** They are exercised by driving `bash -i` and
`zsh -i` with a script on stdin and asserting on the resulting database. Unit
tests cannot catch `PROMPT_COMMAND` chaining, `$?` capture ordering, or the
`HISTCONTROL` interactions, and those are where the bugs are. `zsh -n` and
`bash -n` syntax-check the emitted snippets, which is cheap and worth doing on
every change to `internal/shell`.

**The picker needs a pty and a terminal emulator.** Bubble Tea diff-renders, so
it never re-emits a full frame; capturing output and taking "the last frame"
reconstructs the *first* paint and will convince you that typing does nothing.
Drive it in a pty and reconstruct the screen with a real emulator.


### Performance budgets

These are treated as acceptance criteria, not aspirations. Current measurements
are in the README; to re-check them:

- **Prompt latency** — time `bash -i` running a few hundred no-op commands with
  and without the hooks loaded, take the difference over the count. Compare
  best-of-three; the absolute numbers move with machine load, the delta does not.
- **Ctrl+R first paint** — drive `shcr tui` in a pty against a 500k-row
  database and measure to the first frame byte. Seed with `internal/store`
  directly rather than through the daemon.
- **Query latency** — `TestQueryPerformanceAtScale` in `internal/store`, which
  skips itself under `-race` because the detector's overhead makes wall-clock
  assertions meaningless.
- **Daemon memory** — `systemctl --user show shcr.service -p MemoryCurrent`.


## Conventions

Comments explain **why**, not what. The code says what it does; a comment earns
its place by recording the reason a thing is the way it is — usually a
constraint that is not visible locally, or a simpler approach that was tried and
does not work.

Tests assert behaviour, not implementation, and their names say what breaks
rather than what is exercised.

New user-visible surfaces go through `internal/theme` so the picker, `shcr list`
and the dashboard keep one vocabulary. Status colour is the only saturated
colour in a row, and it means the same thing everywhere.


## Releasing

```sh
CGO_ENABLED=0 go build -trimpath \
  -ldflags="-s -w -X main.version=$(git describe --tags --always)" \
  -o bin/shcr ./cmd/shcr
```

`shcr version` reports the tag, the commit, and whether the tree was dirty. The
commit comes from the build information Go embeds by itself, so even a plain
`go build` says which tree produced it.

## License

By contributing you agree that your contribution is licensed under the MIT
licence in [LICENSE](LICENSE).
