# Syncing shellcrumbs between machines

Everything here assumes `shcr` is installed and recording — see the
[README](../README.md) if it is not.

## How it works, in one paragraph

Each device writes only under its own prefix in the bucket, so no two machines
ever touch the same object: there is no locking, no conflict resolution and no
last-writer-wins to reason about. Events are merged by event id, which the store
already treats as idempotent, so pulling the same batch twice changes nothing.
Batches are encrypted before they leave the machine with a key that never does.

That means the bucket is not a backup you restore from — it is the medium the
machines converge through. A machine rebuilt from scratch pulls the whole
history back out of it, provided you still have the recovery phrase.


## Choosing a backend

**A shared directory** (`--dir`) is a real option, not a fallback. Point it at a
NAS mount, a Dropbox or Syncthing folder, or an `rclone mount` and it works, end
to end encrypted, with no cloud credentials anywhere:

```sh
shcr sync enable --dir /mnt/nas/shcr
```

**Google Cloud Storage** (`--bucket`) is for when the machines have no folder in
common. The rest of this page is mostly about setting that up.

There is no S3 or R2 backend yet.


## Setting up a Google Cloud Storage bucket

### 1. Create the bucket

Any region close to you. **Standard** storage class — the other options cost more
here, not less:

- **Autoclass** charges a per-object management fee. shcr writes one object per
  batch and never deletes anything, so the object count only grows, and at a few
  tens of thousands that fee is larger than the storage itself. A few hundred
  megabytes at Standard is under a cent a month.
- **Nearline / Coldline / Archive** trade cheaper storage for retrieval fees,
  and the retrieval case here is the one that matters most: a rebuilt machine
  pulling its whole history back.

Leave **Hierarchical namespace** off. It is permanent, it buys atomic folder
renames and faster folder listing, and shcr does neither — it lists by prefix
with a delimiter, which flat namespace handles natively.

On the access screen choose **uniform bucket-level access** and leave **public
access prevention** enforced. Skip object versioning: batches are written once
and never modified, so versions would only duplicate storage.

### 2. Give shcr credentials

shcr uses Application Default Credentials and never holds a credential of its
own. Either of these works:

```sh
gcloud auth application-default login          # your own account
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/key.json   # a service account
```

The identity needs to read objects, create them, and **replace** one: the
manifest is rewritten on every push. A create-only role is not enough — batches
will upload and the manifest write will fail. **Storage Object User** on the
bucket covers it; **Storage Object Admin** also works. Neither needs any
bucket-level or project-level role beyond that, and shcr never touches bucket
configuration, IAM or lifecycle — the OAuth scope it asks for cannot.

### 3. Turn it on

```sh
shcr key init                       # once, on your first machine
shcr sync enable --bucket my-bucket --prefix shcr
shcr sync now
```

`--prefix` is optional and nests everything under a folder, so one bucket can
hold more than shcr. `shcr key init` must run in a terminal: it prints the
recovery phrase, and shcr refuses to write that anywhere it could be redirected
into a file.

### 4. Expect the first sync to be the slow one

Cloud Storage caps mutations of a single object at roughly one per second, and
the manifest is rewritten once per batch of 500 events. A backlog therefore
backs off and retries its way through at about a batch a second. On a real
history of 10,895 events — 22 batches — that took **under 15 seconds**. Steady
state is one batch per round and never approaches the limit.


## Adding a second machine

The recovery phrase is the only thing that travels between machines, and it
travels on paper. There is no way to send it through the bucket: the bucket is
what it decrypts.

**On the machine that already works:**

```sh
shcr key show --reveal
```

Write the 24 words down. This must be a terminal — redirected, shcr refuses,
because the file would be created with your umask and readable by anyone on the
machine.

**On the new machine**, install shcr and load the hooks first, then:

```sh
shcr key import                     # paste the same 24 words
shcr sync enable --bucket my-bucket --prefix shcr    # or --dir, matching the first machine
shcr sync now
```

The order matters only in that `sync enable` refuses until a key exists:

```
shcr: no encryption key yet: run `shcr key init` (or `shcr key import` on a second machine) first
```

Do **not** run `shcr key init` on the second machine. It generates a *new* key,
and shcr will refuse anyway — replacing a key orphans everything already in the
bucket, unreadably.

### What you should see

```
$ shcr sync now
pushed 0 event(s), pulled 3 event(s)

$ shcr sync status
sync      gcs backend at gs://my-bucket/shcr
device    019fdac2-916c-7c7b-8c77-0ed38745b36c (this machine)
hostname  shared, so peers can tell which machine is which
pending   0 event(s) waiting to upload
peers
  019fdac2-8f55-71f5-b266-18f319fb4be7   laptop   last synced 2026-08-07 07:46
```

`shcr list` on the new machine now shows the other machine's history, marked
with its hostname. If the peer line says `(unnamed)`, that machine was set up
with `--no-share-hostname`; only the device id crosses in that case.

The new machine's own existing history uploads on the same round, under its own
prefix. Importing its old shell history first (`shcr import`) is worth doing
before the first sync, so it all goes up together.


## Checking it worked

```sh
shcr sync status          # peers, and when each was last seen
shcr list --host laptop   # commands from a specific machine
```

Nothing appearing? `pending N event(s)` in `sync status` means events are queued
locally and the push is failing — run `shcr sync now` in the foreground to see
the error, which the daemon only writes to its log.


## When syncing happens by itself

Never more often than every 30 seconds, never less often than every 3 hours, and
in between on the moments that matter: the daemon starting, a command recorded,
a shell opening or closing, and Ctrl+R. Triggers inside the floor window are
coalesced rather than dropped, so the last command before you close a laptop
still gets pushed. `shcr sync now` and the dashboard's button bypass the floor.


## Troubleshooting

| what you see | what it means |
|---|---|
| `gcs: no credentials (try gcloud auth application-default login...)` | ADC found nothing. Run one of the two commands in step 2. |
| `403 ... does not have storage.objects.list access` | The identity is real but lacks the role. Grant Storage Object User on the bucket. |
| `429 ... exceeded the rate limit for object mutation operations` | Should no longer surface: shcr retries these with backoff. If it does, the backlog is larger than six attempts can clear — re-run `shcr sync now`, which resumes rather than restarting. |
| `sync is not configured` | `shcr sync enable` has not been run on this machine. |
| `no encryption key yet` | Run `shcr key import` (second machine) or `shcr key init` (first). |
| Peers listed but nothing pulled | Normal when the peer has pushed nothing new since your last round. |


## Things worth knowing before you commit to it

- **The phrase cannot be recovered.** Not by us, not by Google. Losing it loses
  everything already uploaded, and the key cannot be replaced without orphaning
  it.
- **One object is not encrypted.** Each device's `manifest.json` is plain JSON:
  a batch key, a timestamp, and the hostname unless you passed
  `--no-share-hostname`. No command text, ever. See the README's
  [privacy section](../README.md#privacy-and-what-actually-protects-you) for
  what the storage provider can infer.
- **Nothing is ever pruned.** An event measured about 340 bytes on a real
  10,000-entry history, so five busy machines for a year come to a few hundred
  megabytes.
- **Removing a machine** means deleting its prefix from the bucket by hand.
  shcr has no command for it, and other machines keep whatever they already
  pulled — the events are theirs now too.
