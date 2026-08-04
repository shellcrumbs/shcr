# Shellcrumbs (`shcr`)

[![CI](https://github.com/shellcrumbs/shcr/actions/workflows/ci.yml/badge.svg)](https://github.com/shellcrumbs/shcr/actions/workflows/ci.yml)

Shell history that records **when a command started**, not only that it ran.

Most other history tools write entries after a command finishes, which means
anything that never finished is simply absent. Shellcrumbs writes a `start`
event before the command runs and an `end` event after, so it can tell you what
is running right now, what failed, and what died along with the terminal that
launched it.

```
$ shcr list
   14:22:07  ✓  git pull                                                    412ms
   14:22:19  ✗  npm run build:prod                            build-server   2.4s   127
   14:24:02  ●  npm run dev
   09:15:40  ◌  ./scripts/migrate.sh --dry-run
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

Open a new shell and start working. Nothing is recorded until the hooks are
loaded, so the shell you ran the install in will not be captured.

To try it without touching any config: `shcr daemon &` in one terminal and
`eval "$(shcr init bash)"` in another.


## Everyday use

Press **Ctrl+R** for the picker. Type to filter, `↑↓` to move, `⏎` to put the
command in your prompt — **editable and unexecuted**: Enter in the picker inserts,
it does not run. `^Y` copies, `^F` cycles the status filter, `esc` cancels.
`SHCR_NO_BIND=1` before the `eval` keeps your own Ctrl+R binding.

```sh
shcr list                      # recent commands, newest last
shcr list -n 50 -q "npm run"   # full-text search
shcr list --status running     # what is in flight right now
shcr list --status orphaned    # what never reported back
shcr list --since 2h
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

---

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


## Getting your history back out

```sh
shcr export                      # JSONL of commands, to stdout
shcr export --format csv         # for a spreadsheet
shcr export --events             # the raw event log — lossless
shcr export -o history.jsonl     # to a file, created 0600
shcr export -q npm --since 720h  # the same filters as `shcr list`
```


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

**Backends.** Only a `file` backend exists: a directory treated as the bucket.
That covers a NAS mount, a synced folder, or an `rclone mount`, all still fully
encrypted. We plan to support other backends in the future.

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


## Redaction

**Redaction rules.** Built-in patterns cover AWS keys, GitHub/GitLab/Slack
tokens, `sk-` API keys, bearer and basic auth headers, passwords in connection
URIs and `--password` flags, JWTs, and private key blocks. Add your own in
`~/.local/share/shcr/redact.conf`:

```
# one rule per line
redact \bcorp-[a-z0-9]{8}\b
skip   ^vault write
```


## The dashboard

```sh
shcr web            # prints a URL carrying a token
shcr web --open     # and opens a browser directly
```

Activity over the last 24 hours, stat cards, a filterable table, a detail
slide-over showing what ran either side of a command in the same shell, and a
settings page — updating live over a server-sent event stream.

It follows your system's light or dark setting, with no toggle to remember and
nothing stored about which one you picked.

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
them as one machine's numbers rather than a benchmark suite:

| | Target | Measured |
|---|---|---|
| Added prompt latency | < 5ms | **1.7ms** (bash and zsh) |
| Ctrl+R to first paint, 500k rows | < 50ms | **21ms** |
| Filtered query, 500k rows | < 50ms | **0.1–0.5ms** typical |
| Daemon resident memory | < 30MB | **8.7MB** |
| Importing 10k history entries | — | **1.7s** |


## Known limits

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
- **Search matches tokens and prefixes, not subsequences.** `npm bui` finds
  `npm run build`; `gitpsh` will not find `git push`. Fuzzy subsequence matching
  is not implemented.
- **Nothing is ever pruned**, and on current evidence nothing needs to be. An
  event measured about 340 bytes on one real 10,000-entry history, so five busy
  machines for a year come to a few hundred megabytes. Deleting old batches
  would cost more than it saves: the bucket is what a rebuilt machine pulls its
  history back from, and any retention window silently truncates a device that was
  offline longer than it. Retention is worth having as a privacy control, not as
  a cost one.
- **fish hooks are written but untested.** bash and zsh are exercised against
  real shells; fish is not.



## Not (yet) built

- **GCS / S3 / R2 backends.** The `Storage` interface is four methods and the
  engine is backend-agnostic, but writing them without a live bucket to test
  against would mean shipping code nobody has run.
- **Command output capture.** The dashboard says so plainly rather than showing
  an empty panel that reads as a bug.
- **macOS.** Everything is pure Go and cross-compiles, but the service
  integration is systemd-only and nothing has been exercised on a Mac.
- **Release packaging.** All four targets build with `CGO_ENABLED=0`
  (linux/darwin × amd64/arm64, ~17MB each), but there is no release pipeline,
  checksums or formula yet.


## Contributing

[CONTRIBUTING.md](CONTRIBUTING.md) covers how it is put together, the invariants
a change has to preserve, and how the parts that cannot be unit-tested — shell
hooks, the picker, the dashboard — are exercised instead.



## Licence

MIT — see [LICENSE](LICENSE). The typefaces embedded in the dashboard are under
the SIL Open Font License 1.1, with their notices in
`internal/web/static/fonts/`. Every dependency linked into the binary is
permissive; `go version -m` on a build lists them.
