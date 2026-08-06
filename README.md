# Shellcrumbs (`shcr`)

[![CI](https://github.com/shellcrumbs/shcr/actions/workflows/ci.yml/badge.svg)](https://github.com/shellcrumbs/shcr/actions/workflows/ci.yml)

Shell history that records **when a command started**, not only that it ran.

Most other history tools write an entry after a command finishes, which means
anything that never finished is simply absent. Shellcrumbs writes a `start`
event before the command runs and an `end` event after, so it can tell you what
is running right now, what failed, and what died along with the terminal that
launched it.

```
$ shcr list
   09:15:40  ◌  ./scripts/migrate.sh --dry-run
   14:22:07  ✓  git pull                                                    412ms
   14:22:19  ✗  npm run build:prod                            build-server   2.4s   127
   14:24:02  ●  npm run dev
```

`●` is running, `✗` failed with exit 127, and `◌` is **orphaned** — a command
whose shell is gone, so no result will ever arrive. That last state is the one no
other tool can show you.

History lives in local SQLite, searchable from a Ctrl+R picker, a web dashboard,
or the command line, and syncs between your machines end-to-end encrypted through
a directory the storage provider cannot read.


## Install

Linux, with a Go toolchain:

```sh
go install github.com/shellcrumbs/shcr/cmd/shcr@latest
echo 'eval "$(shcr init bash)"' >> ~/.bashrc     # or: shcr init zsh
shcr service install                             # run the daemon under systemd
```

That puts the binary in `$(go env GOPATH)/bin`, which is `~/go/bin` unless you
have set `GOPATH` yourself. Add it to your `PATH` if it is not there already.

From a checkout instead:

```sh
git clone https://github.com/shellcrumbs/shcr && cd shcr
go build -o bin/shcr ./cmd/shcr
cp bin/shcr ~/.local/bin/                        # somewhere a rebuild will not replace
```

There is no tagged release yet, so there is nothing to download — see
[Not yet built](#not-yet-built).

Open a new shell and start working. Nothing is recorded until the hooks are
loaded, so the shell you ran the install in will not be captured.

To try it without touching any config: `shcr daemon &` in one terminal and
`eval "$(shcr init bash)"` in another.


## All commands

| | |
|---|---|
| `shcr init <bash\|zsh\|fish>` | print the shell integration to `eval` |
| `shcr tui` | open the picker; prints the chosen command to stdout |
| `shcr list` | show recorded commands |
| `shcr stats` | summarise what has been recorded |
| `shcr web` | serve the dashboard on `127.0.0.1` |
| `shcr import [file...]` | bring in an existing shell history file |
| `shcr export` | write your history out, to stdout or a file |
| `shcr sync now\|status\|enable` | cross-machine sync; `enable` takes `--dir` or `--bucket` |
| `shcr key init\|show\|import` | manage the end-to-end encryption key |
| `shcr redact <id>` | replace a recorded command with a tombstone |
| `shcr rank stats\|forget` | how well the picker's ordering is doing |
| `shcr service install\|status\|units\|uninstall` | run the daemon under systemd |
| `shcr daemon` | run the capture daemon in the foreground |
| `shcr version`, `shcr help` | version, and this list |
| `shcr event start\|end`, `shcr nudge <reason>` | called by the shell hooks; not for typing |

`list`, `stats`, `import`, `export`, `web` and `tui` take `-h` to list their
flags. The rest are subcommand-only and have none; `shcr help` lists them all.


## Everyday use

Press **Ctrl+R** for the picker. Type to filter, `↑↓` and `PgUp`/`PgDn` to move,
`⏎` to put the command in your prompt — **editable and unexecuted**: Enter in the
picker inserts, it does not run. `^Y` copies, `^U` clears what you have typed,
`^F` cycles the status filter (all → running → failed → orphaned), `esc` cancels.

The picker is `shcr tui`; it prints the chosen command to stdout and nothing
else. `SHCR_NO_BIND=1` before the `eval` keeps your own Ctrl+R binding, and you
can then bind `shcr tui` wherever you like.

```sh
shcr list                      # recent commands, newest last
shcr list -n 50 -q "npm run"   # full-text search
shcr list --status running     # what is in flight right now
shcr list --status orphaned    # what never reported back
shcr list --since 2h
shcr list --host build-server --cwd ~/app --session <id>
shcr list --full               # whole multi-line commands, not one line each
shcr stats
shcr redact <id>               # replace one command's text, everywhere
```

Rows read the same in the picker and on the command line, because both use one
renderer. Metadata appears as chips only when it has something to say:

| chip | appears |
|---|---|
| host | only when the command ran on a **different** machine |
| duration | whenever it is known |
| exit code | only when it is **non-zero** — a green tick already says exit 0 |
| directory (`list` only) | only when it differs from your current directory |

Colour turns itself off when piped or redirected and honours `NO_COLOR`.
`--color always` forces it back for `| less -R`; `SHCR_THEME=light` suits a light
terminal.


## How the picker orders results

With nothing typed, the picker shows history newest first — you have just
pressed Ctrl+R, and the thing you want is usually the thing you just did.

Once you type, results are ordered by how well they match **and** how much use
they have had, in that order of priority:

1. **Match quality is a hard gate.** A prefix match always outranks a substring
   match, which always outranks a fuzzy one. Nothing about frequency can promote
   a worse kind of match above a better one.
2. **Within a tier, frecency decides.** A decaying counter with a 72-hour
   half-life, so a command you ran ten times last month sits below one you ran
   twice this morning.
3. **Then context adjusts it.** Same directory, same git repo, same branch, same
   host and same shell session all count for something, as does whether the
   command usually succeeds. A command that has never once worked is pushed
   down; one that failed in the last fifteen minutes is pulled up, because you
   are probably trying to fix it.

Identical commands appear once, with the detail pane saying what the row stands
for (`ran 8× · 8 ok`).

The picker keeps a local record of what you searched for and which result you
took, so the ordering can be measured rather than guessed at. `shcr rank stats`
shows it, `shcr rank forget` empties it, and it never leaves the machine — see
[Privacy](#privacy-and-what-actually-protects-you).


## Bringing in your existing history

```sh
shcr import --dry-run          # find history files, report what would come in
shcr import                    # do it
shcr import ~/.zsh_history     # or name files explicitly
```

Finds bash, zsh and fish histories, sniffs the format from the contents rather
than the filename, and brings them in. Running it twice imports nothing the
second time.

Imported commands are marked `↧` rather than given a status they cannot support.
zsh histories carry real timestamps; bash histories usually carry none, in which
case entries are spaced backwards from the file's modification time to preserve
order and flagged approximate.

Imports run through the same secret filter as live capture.

Because a history file records no exit codes and often no times, imported
commands carry less signal than captured ones, and the picker's ordering knows
less about them than about anything recorded since.


## Getting your history back out

```sh
shcr export                      # JSONL of commands, to stdout
shcr export --format csv         # for a spreadsheet
shcr export --events             # the raw event log — lossless
shcr export -o history.jsonl     # to a file, created 0600
shcr export -q npm --since 720h  # the same filters as `shcr list`
```

`--events` is the form to keep: command rows are derived from those events, so
replaying them into an empty database reconstructs everything, redactions
included.


## Syncing another machine

```sh
# on the first machine
shcr key init                       # prints a 24-word recovery phrase
shcr sync enable --dir /path/to/shared/bucket

# on the second
shcr key import                     # paste the same 24 words
shcr sync enable --dir /path/to/shared/bucket

shcr sync now                       # or let the daemon do it
shcr sync status
```

The bucket only ever holds ciphertext — XChaCha20-Poly1305, a fresh nonce per
batch. The key never leaves the machines it is on. Losing the phrase means losing
access to everything already uploaded, and nobody can recover it for you.

**Backends.** Two. `--dir` treats a directory as the bucket, which covers a NAS
mount, a synced folder or an `rclone mount`. `--bucket` uses Google Cloud
Storage:

```sh
shcr sync enable --bucket my-bucket --prefix shcr   # --prefix is optional
```

Credentials come from Application Default Credentials, so
`gcloud auth application-default login` or a service account key in
`GOOGLE_APPLICATION_CREDENTIALS` both work, and shcr never holds a credential of
its own. The service account needs **Storage Object Admin** on the bucket and
nothing more. Either way the bucket only ever holds ciphertext.

Standard storage class is the one to pick: shcr writes many small objects and
never deletes them, so Autoclass's per-object management fee outgrows the
storage it saves, and the colder classes charge retrieval exactly when a rebuilt
machine pulls its whole history back.

There is no S3 or R2 backend yet — see [Not yet built](#not-yet-built).

**When syncing happens.** Two bounds and a set of triggers, rather than a
polling interval:

- **Never more often than every 30 seconds.** Triggers arriving inside that
  window are coalesced into one sync at the end of it — deferred, not dropped,
  so the last command before you close a laptop still gets pushed.
- **Never less often than every 3 hours**, so a machine nobody touches still
  converges.
- **In between, moments that actually matter trigger it**: the daemon starting,
  a command recorded, a shell opening or closing, and Ctrl+R — you are about to
  read history and you are evidently at the keyboard.

Sitting down at a terminal is when another machine's history is most worth
having, so reacting to that beats polling for it: the cadence is tight while you
work and near-silent while you are away. `shcr sync now` and the dashboard's
button bypass the floor.


## The dashboard

```sh
shcr web            # prints a URL carrying a token
shcr web --open     # and opens a browser directly
```

Activity over the last 24 hours, stat cards, a filterable table, a detail
slide-over showing what ran either side of a command in the same shell, and a
settings page — updating live over a server-sent event stream.

Press <kbd>?</kbd> for the keyboard shortcuts: `/` searches, `j`/`k` and the
arrows move between commands, `⏎` opens one, `Esc` backs out.

It follows your system's light or dark setting, with a toggle in the sidebar to
override it. That choice is the only thing the page keeps in your browser, and
clicking back to "System" removes it again.

It binds `127.0.0.1` (so only accessible on the local machine), and every
`/api` route requires the token in that URL, regenerated on each start.
Localhost is not a boundary on a shared machine: any other account can reach a
loopback port.

The page loads nothing from the internet. Both typefaces are embedded in the
binary, so it renders identically offline and opening your own history tells
nobody about it.

| route | |
|---|---|
| `GET /api/commands` | `q`, `host`, `status`, `session`, `cwd`, `since`, `before`, `limit`, `offset` |
| `GET /api/commands/{id}` | with the commands either side of it in the same shell |
| `POST /api/commands/{id}/redact` | emits a redact event, so it propagates on the next sync |
| `GET /api/stats` | today's counts and a 24-hour histogram |
| `GET /api/hosts`, `GET /api/devices` | for the filters and the device list |
| `GET`/`PATCH /api/settings` | sync toggles only |
| `POST /api/sync` | runs a sync round |
| `GET /api/events` | live stream of row changes |


## Redaction

Built-in patterns cover AWS keys, GitHub/GitLab/Slack tokens, `sk-` API keys,
bearer and basic auth headers, passwords in connection URIs and `--password`
flags, JWTs, and private key blocks. Add your own in
`~/.local/share/shcr/redact.conf`:

```
# one rule per line
redact \bcorp-[a-z0-9]{8}\b
skip   ^vault write
```

`redact` replaces the matched text; `skip` drops the command entirely, so
nothing about it is stored.

`shcr redact <id>` handles the one that got away: it appends an event, so the
text is replaced on every machine on the next sync, in the search index as well
as the table.


## Running it as a service

```sh
shcr service install     # writes both units, enables and starts them
shcr service status
shcr service units       # print them without installing
shcr service uninstall   # removes the units; leaves your history alone
```

Two user-scope units. The socket is created at login so the first command of a
session reaches the daemon instead of falling back to the spool file, and systemd
owns creating and removing it. The daemon is started *by* the socket but does not
stop with it — the orphan sweep runs every minute and sync on its own schedule.

By default the daemon stops when your last session ends, so a machine you only
ssh into will not sweep orphans or sync while you are away.
`sudo loginctl enable-linger $USER` keeps it running.


## Privacy, and what actually protects you

- **Command output is not captured.** Only the command line, its metadata and
  its result.
- **The picker keeps a local record of what you searched for** — the query, the
  command you chose and where it ranked — because there is no other way to tell
  whether a change to the ordering helped or merely changed it. It is not an
  event, so it never syncs, and not a command, so `shcr export` does not carry
  it. `shcr rank stats` shows what it is for, `shcr rank forget` empties it, and
  `ranking.log_acceptances: false` in the config turns it off.
- **Secrets are filtered before anything is written** — in the sender, so they
  never reach the spool file either, and again in the daemon as a backstop.
- **`shcr redact` propagates.** It appends an event, so the text is replaced on
  every machine, in the search index as well as the table.
- **Everything on disk is yours alone.** Data and state directories `0700`; the
  database, its write-ahead log, the spool, the key file and the config `0600`;
  the daemon socket `0600` in a per-user runtime directory.
- **Command text never travels through the argument list.**
  `/proc/<pid>/cmdline` is world-readable and `/proc/<pid>/environ` is not, so
  the hooks pass command text through the environment. This matters most for
  shell builtins — `export TOKEN=...` appears in no process list on its own, so
  recording it as an argument would create an exposure that did not exist.
- **A leading space keeps a command out**, but only where your shell is set up
  for it: bash needs `HISTCONTROL=ignorespace` (Debian and Ubuntu set
  `ignoreboth` by default) and zsh needs `setopt hist_ignore_space`, which is
  off unless you turned it on. Where it applies it is the reliable way to keep
  something out; pattern matching is a safety net, not a guarantee.

Two things it does **not** protect against. `shcr web --open` hands the token to
the browser launcher, where it is visible in that process's arguments and
afterwards in browser history — on a shared machine, run plain `shcr web` and
paste the URL yourself. And the storage provider still sees object sizes,
timestamps and how many machines you have, even though it cannot read contents.


## What it costs

Measured on one Linux laptop, which is the caveat that matters: only the query
row is pinned by a test in the tree (`TestQueryPerformanceAtScale`). The others
come from the manual procedures in [CONTRIBUTING.md](CONTRIBUTING.md), so treat
them as one machine's numbers rather than a benchmark suite.

| | Target | Measured |
|---|---|---|
| Added prompt latency | < 5ms | **1.7ms** (bash and zsh) |
| Ctrl+R to first paint, 500k rows | < 50ms | **30ms** |
| Filtered query, 500k rows | < 50ms | **0.1–0.5ms** typical |
| Ranking, per keystroke, 20k distinct commands | < 25ms | **pinned by `TestRankingCostPerKeystroke`** |
| Daemon resident memory | < 30MB | **8.7MB** |
| Importing 10k history entries | — | **1.7s** |


## Known limits

- **Startup pays a terminal query.** Bubble Tea, which the picker is built on,
  queries the terminal for its background colour from a package `init()` — so
  every `shcr` command does it, whether or not it draws anything. Terminals
  answer in about a millisecond. One that does *not* answer costs a five-second
  stall before any output. `tmux`, `screen` and `TERM=dumb` are skipped, `CI` is
  skipped, and piped or redirected output is skipped; a bare pty is not, which
  is worth knowing before scripting `shcr` under one.
- **`shcr list -q` matches tokens and prefixes; the picker also matches
  subsequences.** In the picker `gitpsh` finds `git push`. `shcr list -q` goes
  through SQLite's full-text index, where it does not: `-q "npm run"` and
  `-q bui` work, `-q gitpsh` finds nothing.
- **`^F` does not cycle through `completed`.** The filter goes all → running →
  failed → orphaned. `shcr list --status completed` does work.
- **Multiline fidelity under bash.** bash normalises leading tabs on continuation
  lines unless `shopt -s lithist` is set, so a tab-indented heredoc does not
  round-trip byte-identically. Storage is exact — the loss is upstream, in what
  bash will tell us. zsh hands over the raw buffer and is unaffected.
- **Aliased repeats under `HISTCONTROL=ignoredups`.** Telling a collapsed
  duplicate from a deliberately hidden `ignorespace` command relies on comparing
  `$BASH_COMMAND` to the history line; alias expansion breaks that match, so a
  repeated *aliased* command may be skipped. Erring this way keeps `ignorespace`
  honest, which is the direction that matters.
- **Backgrounded commands never complete.** `sleep 30 &` correctly stays
  `running` rather than being marked done when the prompt returns, but nothing
  reports its eventual exit, so it stays that way until the shell exits and the
  sweep marks it orphaned.
- **Redaction is pattern-based.** It catches the credential shapes listed above;
  a password passed positionally has no distinctive shape and will not be caught.
- **Nothing is ever pruned**, and on current evidence nothing needs to be. An
  event measured about 340 bytes on one real 10,000-entry history, so five busy
  machines for a year come to a few hundred megabytes. Deleting old batches
  would cost more than it saves: the bucket is what a rebuilt machine pulls its
  history back from, and any retention window silently truncates a device that was
  offline longer than it. Retention is worth having as a privacy control, not as
  a cost one.
- **fish hooks are written but untested.** bash and zsh are exercised against
  real shells; fish is not.
- **The GCS backend has not been run against a real bucket.** It is covered by
  a conformance suite that runs every assertion against both backends, and by a
  fake that copies the API semantics that matter — `startOffset` is inclusive,
  listings page — but a fake agrees with whatever you believed when you wrote
  it. The directory backend is the one with real mileage.


## Not yet built

- **S3 / R2 backends.** The `Storage` interface is four methods and the engine
  is backend-agnostic, so these are a day's work each; nobody has needed one
  yet. GCS exists.
- **A tagged release.** The pipeline exists — GoReleaser builds static
  `CGO_ENABLED=0` binaries for linux amd64 and arm64 with checksums, on tag —
  but no tag has been pushed, so `go install` or a checkout is the only way in.
  No Homebrew formula or distribution package.
- **Command output capture.** Only the command line and its result are recorded.
- **macOS and Windows.** Everything is pure Go, but the service integration is
  systemd-only, the socket and `/proc` assumptions are Linux, and nothing has
  been exercised elsewhere. The release build is deliberately Linux-only for
  that reason.


## Contributing

[CONTRIBUTING.md](CONTRIBUTING.md) covers how it is put together, the invariants
a change has to preserve, and how the parts that cannot be unit-tested — shell
hooks, the picker, the dashboard — are exercised instead.


## Licence

MIT — see [LICENSE](LICENSE). The typefaces embedded in the dashboard are under
the SIL Open Font License 1.1, with their notices in
`internal/web/static/fonts/`. Every dependency linked into the binary is
permissive; `go version -m` on a build lists them.
