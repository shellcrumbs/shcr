package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shellcrumbs/shcr/internal/crypto"
	"github.com/shellcrumbs/shcr/internal/event"
	"github.com/shellcrumbs/shcr/internal/store"
)

// device is one simulated machine: its own database, its own device id, sharing
// one bucket and one key with the others.
type device struct {
	id     string
	host   string
	store  *store.Store
	engine *Engine
	seq    int
}

func newDevice(t *testing.T, id, host string, bucket Storage, key crypto.Key) *device {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), id+".db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	d := &device{id: id, host: host, store: st}
	d.engine = &Engine{
		Store: st, Storage: bucket, Key: key,
		DeviceID: id, Hostname: host, ShareHostname: true,
	}
	return d
}

// run records a command the way the daemon would: a start event and an end
// event, both locally produced.
func (d *device) run(t *testing.T, command string, exit int, at int64) string {
	t.Helper()
	d.seq++
	cmdID := fmt.Sprintf("%s-cmd-%d", d.id, d.seq)

	start, err := event.New(cmdID, d.id, event.TypeStart, event.StartPayload{
		Command: command, Hostname: d.host, SessionID: d.id + "-sess",
		Cwd: "/home/u/app", Shell: "bash", StartTime: at, PGID: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	start.CreatedAt = at
	if _, err := d.store.AppendEvent(start); err != nil {
		t.Fatal(err)
	}

	end, err := event.New(cmdID, d.id, event.TypeEnd, event.EndPayload{EndTime: at + 100, ExitCode: exit})
	if err != nil {
		t.Fatal(err)
	}
	end.CreatedAt = at + 100
	if _, err := d.store.AppendEvent(end); err != nil {
		t.Fatal(err)
	}
	return cmdID
}

func (d *device) sync(t *testing.T) {
	t.Helper()
	if _, _, err := d.engine.SyncOnce(context.Background()); err != nil {
		t.Fatalf("%s sync: %v", d.id, err)
	}
}

func (d *device) commands(t *testing.T) map[string]string {
	t.Helper()
	rows, err := d.store.QueryCommands(store.Filter{Limit: 100000})
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string, len(rows))
	for _, c := range rows {
		out[c.ID] = fmt.Sprintf("%s|%s|%s|%s", c.Command, c.Hostname, c.Status, c.Cwd)
	}
	return out
}

func newBucket(t *testing.T) (*FileStorage, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "bucket")
	fs, err := NewFileStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	return fs, root
}

func TestTwoDevicesConverge(t *testing.T) {
	bucket, _ := newBucket(t)
	key, _ := crypto.GenerateKey()

	laptop := newDevice(t, "dev-laptop", "laptop-mac", bucket, key)
	server := newDevice(t, "dev-server", "build-server", bucket, key)

	laptop.run(t, "npm run build:prod", 0, 1000)
	laptop.run(t, "git push", 1, 2000)
	server.run(t, "make deploy", 0, 3000)

	// One round each way is enough: push then pull.
	laptop.sync(t)
	server.sync(t)
	laptop.sync(t)

	l, s := laptop.commands(t), server.commands(t)
	if len(l) != 3 || len(s) != 3 {
		t.Fatalf("expected 3 commands on both sides, got laptop=%d server=%d", len(l), len(s))
	}
	for id, want := range l {
		if s[id] != want {
			t.Errorf("command %s differs:\n laptop %q\n server %q", id, want, s[id])
		}
	}
	// Metadata must survive the trip, not just the text.
	for _, v := range s {
		if strings.Contains(v, "npm run build:prod") && !strings.Contains(v, "laptop-mac") {
			t.Errorf("hostname lost in transit: %s", v)
		}
	}
}

// A machine offline for days must catch up fully.
func TestOfflineDeviceCatchesUp(t *testing.T) {
	bucket, _ := newBucket(t)
	key, _ := crypto.GenerateKey()

	active := newDevice(t, "dev-active", "workstation", bucket, key)
	away := newDevice(t, "dev-away", "laptop", bucket, key)

	// Three days of work, pushed as many separate batches.
	const days = 3
	const perDay = 20
	at := int64(1_700_000_000_000)
	for d := range days {
		for i := range perDay {
			active.run(t, fmt.Sprintf("day%d cmd%d", d, i), 0, at)
			at += 1000
		}
		active.sync(t)
	}

	if _, err := away.engine.PullOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := away.commands(t)
	if len(got) != days*perDay {
		t.Fatalf("caught up with %d commands, want %d", len(got), days*perDay)
	}
}

// Nothing readable may reach the bucket.
func TestBucketHoldsOnlyCiphertext(t *testing.T) {
	bucket, root := newBucket(t)
	key, _ := crypto.GenerateKey()
	d := newDevice(t, "dev-a", "secret-host", bucket, key)

	const secret = "deploy --to production-cluster-7"
	d.run(t, secret, 0, 1000)
	d.sync(t)

	var checked int
	err := filepath.WalkDir(root, func(p string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		checked++
		rel, _ := filepath.Rel(root, p)
		if bytes.Contains(body, []byte(secret)) {
			t.Errorf("command text found in plaintext in %s", rel)
		}
		if bytes.Contains(body, []byte("production-cluster")) {
			t.Errorf("command fragment found in plaintext in %s", rel)
		}
		if strings.HasSuffix(p, ".enc") && bytes.Contains(body, []byte("/home/u/app")) {
			t.Errorf("working directory leaked in plaintext in %s", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 2 {
		t.Fatalf("expected at least a manifest and a batch, saw %d objects", checked)
	}
}

// The manifest is deliberately readable, so be explicit about what it exposes.
func TestManifestExposesOnlyPointerData(t *testing.T) {
	bucket, _ := newBucket(t)
	key, _ := crypto.GenerateKey()
	d := newDevice(t, "dev-a", "build-server", bucket, key)
	d.run(t, "terraform apply -auto-approve", 0, 1000)
	d.sync(t)

	raw, err := bucket.Get(context.Background(), manifestKey("dev-a"))
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.LatestBatch == "" {
		t.Fatalf("manifest not populated: %+v", m)
	}
	if strings.Contains(string(raw), "terraform") {
		t.Error("manifest leaked command text")
	}

	// With sharing off, not even the hostname goes up.
	d.engine.ShareHostname = false
	d.run(t, "echo hi", 0, 2000)
	d.sync(t)
	raw, _ = bucket.Get(context.Background(), manifestKey("dev-a"))
	if strings.Contains(string(raw), "build-server") {
		t.Error("hostname present despite ShareHostname=false")
	}
}

func TestRepeatedPullIsIdempotent(t *testing.T) {
	bucket, _ := newBucket(t)
	key, _ := crypto.GenerateKey()
	a := newDevice(t, "dev-a", "a", bucket, key)
	b := newDevice(t, "dev-b", "b", bucket, key)

	a.run(t, "make test", 0, 1000)
	a.sync(t)

	first, err := b.engine.PullOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first == 0 {
		t.Fatal("first pull applied nothing")
	}
	before := b.commands(t)

	for range 3 {
		n, err := b.engine.PullOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("re-pull applied %d events, want 0", n)
		}
	}
	if len(b.commands(t)) != len(before) {
		t.Error("re-pulling changed the command set")
	}
}

// A device must never republish events it received from a peer, or every
// machine ends up echoing every other machine's history into the bucket.
func TestPulledEventsAreNotRepublished(t *testing.T) {
	bucket, _ := newBucket(t)
	key, _ := crypto.GenerateKey()
	a := newDevice(t, "dev-a", "a", bucket, key)
	b := newDevice(t, "dev-b", "b", bucket, key)

	a.run(t, "one", 0, 1000)
	a.run(t, "two", 0, 2000)
	a.sync(t)
	b.sync(t) // pulls a's events

	pushed, err := b.engine.PushOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pushed != 0 {
		t.Fatalf("device b uploaded %d events it did not produce", pushed)
	}
	keys, err := bucket.List(context.Background(), batchPrefix("dev-b"), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("device b wrote %d batches despite producing nothing: %v", len(keys), keys)
	}
}

func TestWrongKeyCannotRead(t *testing.T) {
	bucket, _ := newBucket(t)
	keyA, _ := crypto.GenerateKey()
	keyB, _ := crypto.GenerateKey()

	a := newDevice(t, "dev-a", "a", bucket, keyA)
	a.run(t, "secret command", 0, 1000)
	a.sync(t)

	intruder := newDevice(t, "dev-x", "x", bucket, keyB)
	n, err := intruder.engine.PullOnce(context.Background())
	// A key that decrypts nothing must be reported, not reported as a quiet
	// success: it is the state where `sync now` printing "pulled 0" and exiting
	// 0 is the most misleading thing the tool could do.
	if err == nil {
		t.Error("pulling every peer with the wrong key reported success")
	}
	if n != 0 {
		t.Fatal("events were applied with the wrong key")
	}
	if len(intruder.commands(t)) != 0 {
		t.Fatal("data leaked to a device with the wrong key")
	}
}

// A batch we cannot read must not be skipped over: advancing past it would lose
// those events for good.
// The other half of reporting a total failure: one broken peer among working
// ones stays a logged warning, or a single machine that once uploaded a corrupt
// batch would put every other machine's sync into permanent backoff.
func TestOneBrokenPeerDoesNotFailAPullThatOtherwiseWorked(t *testing.T) {
	bucket, root := newBucket(t)
	key, _ := crypto.GenerateKey()
	good := newDevice(t, "dev-good", "g", bucket, key)
	bad := newDevice(t, "dev-bad", "b", bucket, key)
	reader := newDevice(t, "dev-read", "r", bucket, key)

	good.run(t, "the command that should arrive", 0, 1000)
	good.sync(t)
	bad.run(t, "unreadable", 0, 2000)
	bad.sync(t)

	keys, _ := bucket.List(context.Background(), batchPrefix("dev-bad"), "")
	if len(keys) != 1 {
		t.Fatalf("expected one batch from dev-bad, got %v", keys)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(keys[0])), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}

	n, err := reader.engine.PullOnce(context.Background())
	if err != nil {
		t.Fatalf("a working peer alongside a broken one should still succeed: %v", err)
	}
	if n == 0 {
		t.Error("nothing was pulled from the peer that was fine")
	}
	var found bool
	for _, v := range reader.commands(t) {
		if strings.Contains(v, "the command that should arrive") {
			found = true
		}
	}
	if !found {
		t.Error("the good peer's command did not arrive")
	}
}

// A single batch per cycle plus a thirty second floor means an imported history
// trickles out for over an hour. Push has to drain what is pending.
func TestPushDrainsABacklogRatherThanOneBatchPerCycle(t *testing.T) {
	bucket, _ := newBucket(t)
	key, _ := crypto.GenerateKey()
	a := newDevice(t, "dev-a", "a", bucket, key)
	b := newDevice(t, "dev-b", "b", bucket, key)

	// Two events per command, so this clears maxEventsPerBatch several times.
	const commands = maxEventsPerBatch // 2x that many events
	for i := 0; i < commands; i++ {
		a.run(t, fmt.Sprintf("command number %d", i), 0, int64(1000+i))
	}

	pushed, err := a.engine.PushOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := commands * 2; pushed != want {
		t.Errorf("pushed %d events in one cycle, want all %d", pushed, want)
	}
	left, err := a.store.UnsyncedEvents(a.id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("%d events still pending after a push", len(left))
	}

	keys, _ := bucket.List(context.Background(), batchPrefix("dev-a"), "")
	if len(keys) < 2 {
		t.Errorf("a backlog should span several batches, got %d", len(keys))
	}
	// The batches must still be readable in order by a peer.
	if _, err := b.engine.PullOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(b.commands(t)); got != commands {
		t.Errorf("peer received %d commands, want %d", got, commands)
	}
}

// The loop's timer and the dashboard's button call SyncOnce from different
// goroutines. Without serialisation both read the same pending events, upload
// them as two batches, and derive a batch key from the same manifest — so one
// silently overwrites the other's pointer.
func TestConcurrentSyncCyclesDoNotLoseABatch(t *testing.T) {
	bucket, _ := newBucket(t)
	key, _ := crypto.GenerateKey()
	a := newDevice(t, "dev-a", "a", bucket, key)
	b := newDevice(t, "dev-b", "b", bucket, key)

	for i := 0; i < 20; i++ {
		a.run(t, fmt.Sprintf("concurrent %d", i), 0, int64(1000+i))
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := a.engine.SyncOnce(context.Background()); err != nil {
				t.Errorf("concurrent sync: %v", err)
			}
		}()
	}
	wg.Wait()

	if _, err := b.engine.PullOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(b.commands(t)); got != 20 {
		t.Errorf("peer received %d commands after concurrent pushes, want 20", got)
	}
}

// The complement of not advancing past a bad batch: the batches read before it
// must not be forgotten either. Dropping that progress costs nothing in
// correctness, since merging is idempotent, but it means every later sync
// re-lists from zero and re-fetches and re-decrypts the good batches again, for
// as long as the bad one stays unreadable.
func TestProgressBeforeAnUnreadableBatchIsKept(t *testing.T) {
	bucket, root := newBucket(t)
	key, _ := crypto.GenerateKey()
	a := newDevice(t, "dev-a", "a", bucket, key)
	b := newDevice(t, "dev-b", "b", bucket, key)

	for i, text := range []string{"first", "second", "third"} {
		a.run(t, text, 0, int64(1000+i*1000))
		a.sync(t)
	}
	keys, _ := bucket.List(context.Background(), batchPrefix("dev-a"), "")
	if len(keys) != 3 {
		t.Fatalf("expected 3 batches, got %d", len(keys))
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(keys[2])), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}

	applied, err := b.engine.PullOnce(context.Background())
	if err == nil {
		t.Fatal("the unreadable batch should be reported")
	}
	if applied == 0 {
		t.Fatal("setup: nothing was applied before the bad batch")
	}

	cur, err := b.store.Cursor("dev-a")
	if err != nil {
		t.Fatal(err)
	}
	if cur.LastBatchKey != keys[1] {
		t.Errorf("cursor = %q, want the last good batch %q", cur.LastBatchKey, keys[1])
	}

	// A second attempt must have nothing left to redo before it reaches the
	// bad batch again.
	again, err := b.engine.PullOnce(context.Background())
	if err == nil {
		t.Error("the batch is still unreadable, so it should still be reported")
	}
	if again != 0 {
		t.Errorf("re-applied %d events that were already stored", again)
	}
	if got := len(b.commands(t)); got != 2 {
		t.Errorf("b holds %d commands, want the 2 from the readable batches", got)
	}
}

func TestCursorDoesNotAdvancePastAnUnreadableBatch(t *testing.T) {
	bucket, root := newBucket(t)
	key, _ := crypto.GenerateKey()
	a := newDevice(t, "dev-a", "a", bucket, key)
	b := newDevice(t, "dev-b", "b", bucket, key)

	a.run(t, "first", 0, 1000)
	a.sync(t)
	a.run(t, "second", 0, 2000)
	a.sync(t)

	// Corrupt the earlier batch.
	keys, _ := bucket.List(context.Background(), batchPrefix("dev-a"), "")
	if len(keys) < 2 {
		t.Fatalf("expected two batches, got %v", keys)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(keys[0])), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}

	// dev-a is the only peer, so its failure is the whole pull failing.
	if _, err := b.engine.PullOnce(context.Background()); err == nil {
		t.Error("a peer whose batch cannot be read should be reported")
	}
	cur, err := b.store.Cursor("dev-a")
	if err != nil {
		t.Fatal(err)
	}
	if cur.LastBatchKey >= keys[0] {
		t.Fatalf("cursor advanced past the unreadable batch (%s >= %s)", cur.LastBatchKey, keys[0])
	}

	// Once the batch is readable again the device catches up completely.
	sealed, err := crypto.Encrypt(key, mustBatchOf(t, a, "first"))
	if err != nil {
		t.Fatal(err)
	}
	if err := bucket.Put(context.Background(), keys[0], sealed); err != nil {
		t.Fatal(err)
	}
	if _, err := b.engine.PullOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(b.commands(t)); got != 2 {
		t.Fatalf("after recovery b has %d commands, want 2", got)
	}
}

// mustBatchOf rebuilds the JSONL for the events of the command whose text
// matches, so a corrupted batch can be restored in the test above.
func mustBatchOf(t *testing.T, d *device, text string) []byte {
	t.Helper()
	rows, err := d.store.QueryCommands(store.Filter{Text: text, Limit: 10})
	if err != nil || len(rows) == 0 {
		t.Fatalf("locating %q: %v", text, err)
	}
	evRows, err := d.store.DB().Query(
		`SELECT event_id, command_id, device_id, type, payload, created_at
		   FROM events WHERE command_id = ? ORDER BY created_at`, rows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer evRows.Close()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for evRows.Next() {
		var ev event.Event
		var typ, payload string
		if err := evRows.Scan(&ev.EventID, &ev.CommandID, &ev.DeviceID, &typ, &payload, &ev.CreatedAt); err != nil {
			t.Fatal(err)
		}
		ev.Type = event.Type(typ)
		ev.Payload = json.RawMessage(payload)
		if err := enc.Encode(ev); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

// Redaction has to reach the other machines, or redacting locally while the
// secret sits in every peer's copy is worse than useless.
func TestRedactionPropagates(t *testing.T) {
	bucket, _ := newBucket(t)
	key, _ := crypto.GenerateKey()
	a := newDevice(t, "dev-a", "a", bucket, key)
	b := newDevice(t, "dev-b", "b", bucket, key)

	cmdID := a.run(t, "curl -H 'Authorization: leaked-token-value'", 0, 1000)
	a.sync(t)
	b.sync(t)

	if !strings.Contains(fmt.Sprint(b.commands(t)), "leaked-token-value") {
		t.Fatal("setup: b should have the unredacted text at this point")
	}

	// Machine A redacts it.
	rev, err := event.New(cmdID, a.id, event.TypeRedact, map[string]any{"reason": "manual"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.AppendEvent(rev); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	b.sync(t)

	all := fmt.Sprint(b.commands(t))
	if strings.Contains(all, "leaked-token-value") {
		t.Fatal("the secret survived on machine b after redaction synced")
	}
	if !strings.Contains(all, event.RedactedMarker) {
		t.Fatalf("expected a redaction marker on b, got %s", all)
	}
	// The row itself must remain, so the history is not silently rewritten.
	if len(b.commands(t)) != 1 {
		t.Fatal("redaction should keep the row, only replace the text")
	}
}

// Several machines, interleaved work and sync rounds in random order, all
// converging on the same history.
func TestManyDevicesConverge(t *testing.T) {
	bucket, _ := newBucket(t)
	key, _ := crypto.GenerateKey()

	const n = 5
	devices := make([]*device, n)
	for i := range devices {
		devices[i] = newDevice(t, fmt.Sprintf("dev-%d", i), fmt.Sprintf("host-%d", i), bucket, key)
	}

	rng := rand.New(rand.NewPCG(42, 1))
	at := int64(1_700_000_000_000)
	for round := range 40 {
		d := devices[rng.IntN(n)]
		switch rng.IntN(3) {
		case 0, 1:
			d.run(t, fmt.Sprintf("cmd from %s round %d", d.id, round), rng.IntN(2), at)
			at += 1000
		default:
			// A sync at an arbitrary moment, which is the realistic case.
			if _, _, err := d.engine.SyncOnce(context.Background()); err != nil {
				t.Fatalf("%s: %v", d.id, err)
			}
		}
	}

	// Settle: everyone pushes, then everyone pulls twice so late batches land.
	for range 2 {
		for _, d := range devices {
			d.sync(t)
		}
	}

	want := devices[0].commands(t)
	if len(want) == 0 {
		t.Fatal("simulation produced no commands")
	}
	for _, d := range devices[1:] {
		got := d.commands(t)
		if len(got) != len(want) {
			t.Errorf("%s has %d commands, device 0 has %d", d.id, len(got), len(want))
			continue
		}
		for id, v := range want {
			if got[id] != v {
				t.Errorf("%s disagrees on %s:\n want %q\n got  %q", d.id, id, v, got[id])
			}
		}
	}
	t.Logf("%d devices converged on %d commands", n, len(want))
}

func TestFileStorageBasics(t *testing.T) {
	bucket, _ := newBucket(t)
	ctx := context.Background()

	if _, err := bucket.Get(ctx, "devices/nope/manifest.json"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("missing key should give ErrNotExist, got %v", err)
	}
	if keys, err := bucket.List(ctx, "devices/", ""); err != nil || len(keys) != 0 {
		t.Fatalf("listing an empty bucket: %v %v", keys, err)
	}

	if err := bucket.Put(ctx, "devices/a/batches/2.enc", []byte("two")); err != nil {
		t.Fatal(err)
	}
	if err := bucket.Put(ctx, "devices/a/batches/1.enc", []byte("one")); err != nil {
		t.Fatal(err)
	}
	keys, err := bucket.List(ctx, "devices/a/batches/", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] >= keys[1] {
		t.Fatalf("List must return sorted keys, got %v", keys)
	}
	if err := bucket.Put(ctx, "../escape", []byte("x")); err == nil {
		t.Error("a key escaping the root should be refused")
	}
}

// Readers track a peer as a single "last key seen" and skip anything at or
// below it, so a batch key that sorts backwards is never read again. Wall
// clocks do step backwards — NTP corrections, restored snapshots, dead coin
// cells — so the writer keeps its own keys ordered regardless of the clock.
func TestBatchKeysStayOrderedWhenTheClockStepsBackwards(t *testing.T) {
	bucket, _ := newBucket(t)
	key, _ := crypto.GenerateKey()
	a := newDevice(t, "dev-a", "a", bucket, key)
	b := newDevice(t, "dev-b", "b", bucket, key)

	a.run(t, "before the clock jumped", 0, 1000)
	a.sync(t)

	// Push again with a clock a decade in the past.
	a.run(t, "after the clock jumped", 0, 2000)
	m, err := a.engine.loadManifest(context.Background(), "dev-a")
	if err != nil {
		t.Fatal(err)
	}
	past := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	newKey, err := a.engine.newBatchKey(past, m.LatestBatch)
	if err != nil {
		t.Fatal(err)
	}
	if newKey <= m.LatestBatch {
		t.Fatalf("key went backwards despite the guard:\n  previous %s\n  new      %s", m.LatestBatch, newKey)
	}

	a.sync(t)
	if _, err := b.engine.PullOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(b.commands(t)); got != 2 {
		t.Fatalf("peer sees %d commands, want 2 — a batch was skipped for good", got)
	}
}

func TestBatchKeyTimestampRoundTrips(t *testing.T) {
	e := &Engine{DeviceID: "dev-a"}
	when := time.Date(2026, 8, 2, 14, 30, 0, 500_000_000, time.UTC)
	key, err := e.newBatchKey(when, "")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := batchKeyTime(key)
	if !ok || !got.Equal(when) {
		t.Fatalf("batchKeyTime(%q) = %v, %v; want %v", key, got, ok, when)
	}
	if _, ok := batchKeyTime("devices/x/batches/not-a-timestamp.jsonl.enc"); ok {
		t.Error("a malformed key should not parse")
	}
	if _, ok := batchKeyTime(""); ok {
		t.Error("an empty key should not parse")
	}
}

// Two pushes inside the same millisecond must still produce ordered keys.
// Without that, ordering falls to the random suffix and roughly half the time
// the second batch sorts below the first — where a reader's cursor will skip it
// permanently.
func TestBatchKeysOrderWithinTheSameMillisecond(t *testing.T) {
	e := &Engine{DeviceID: "dev-a"}
	instant := time.Date(2026, 8, 2, 14, 30, 0, 123_456_789, time.UTC)

	previous := ""
	for i := range 50 {
		// Same wall-clock instant every time, as a burst of pushes would see.
		key, err := e.newBatchKey(instant, previous)
		if err != nil {
			t.Fatal(err)
		}
		if previous != "" && key <= previous {
			t.Fatalf("batch %d sorts at or below its predecessor:\n  previous %s\n  new      %s", i, previous, key)
		}
		previous = key
	}
}

// The same, through the real push path.
func TestConsecutivePushesProduceOrderedBatches(t *testing.T) {
	bucket, _ := newBucket(t)
	key, _ := crypto.GenerateKey()
	a := newDevice(t, "dev-a", "a", bucket, key)

	for i := range 6 {
		a.run(t, fmt.Sprintf("command %d", i), 0, int64(1000+i*10))
		a.sync(t)
	}
	keys, err := bucket.List(context.Background(), batchPrefix("dev-a"), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) < 2 {
		t.Fatalf("expected several batches, got %v", keys)
	}
	// List sorts lexically; creation order must match, or a reader skips work.
	for i := 1; i < len(keys); i++ {
		if keys[i] <= keys[i-1] {
			t.Fatalf("batch %d is not after %d:\n  %s\n  %s", i, i-1, keys[i-1], keys[i])
		}
	}
}

// countingStorage records how much the engine asked the backend to enumerate,
// which is the cost that used to grow with history forever.
type countingStorage struct {
	Storage
	lists       int
	keysListed  int
	children    int
	childReturn int
}

func (c *countingStorage) List(ctx context.Context, prefix, after string) ([]string, error) {
	keys, err := c.Storage.List(ctx, prefix, after)
	c.lists++
	c.keysListed += len(keys)
	return keys, err
}

func (c *countingStorage) Children(ctx context.Context, prefix string) ([]string, error) {
	kids, err := c.Storage.Children(ctx, prefix)
	c.children++
	c.childReturn += len(kids)
	return kids, err
}

// A pull must cost what is new, not what exists. Listing a peer's whole batch
// history on every sync is fine for a week and ruinous after a few years, and
// the cursor already says exactly where to resume from.
func TestPullCostDoesNotGrowWithHistory(t *testing.T) {
	bucket, _ := newBucket(t)
	key, _ := crypto.GenerateKey()
	a := newDevice(t, "dev-a", "a", bucket, key)

	counting := &countingStorage{Storage: bucket}
	b := newDevice(t, "dev-b", "b", bucket, key)
	b.engine.Storage = counting

	// Build up a long history, one batch per round.
	const rounds = 40
	for i := range rounds {
		a.run(t, fmt.Sprintf("command %d", i), 0, int64(1000+i*10))
		a.sync(t)
	}
	if _, err := b.engine.PullOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	coldKeys := counting.keysListed
	if coldKeys < rounds {
		t.Fatalf("cold start should have enumerated the backlog, saw %d keys", coldKeys)
	}

	// Steady state: one new batch at a time.
	for i := range 5 {
		counting.keysListed, counting.lists = 0, 0
		a.run(t, fmt.Sprintf("later %d", i), 0, int64(9000+i*10))
		a.sync(t)
		if _, err := b.engine.PullOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if counting.keysListed > 3 {
			t.Errorf("steady-state pull enumerated %d keys against a %d-batch history; "+
				"the listing is not bounded by the cursor", counting.keysListed, rounds+i)
		}
	}
}

// Peer discovery must cost the number of devices, not the number of objects.
func TestPeerDiscoveryDoesNotEnumerateEveryObject(t *testing.T) {
	bucket, _ := newBucket(t)
	key, _ := crypto.GenerateKey()
	a := newDevice(t, "dev-a", "a", bucket, key)

	for i := range 25 {
		a.run(t, fmt.Sprintf("command %d", i), 0, int64(1000+i*10))
		a.sync(t)
	}

	counting := &countingStorage{Storage: bucket}
	b := newDevice(t, "dev-b", "b", bucket, key)
	b.engine.Storage = counting

	peers, err := b.engine.Peers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0] != "dev-a" {
		t.Fatalf("peers = %v, want [dev-a]", peers)
	}
	if counting.children != 1 {
		t.Errorf("expected one delimited listing, got %d", counting.children)
	}
	if counting.childReturn > 2 {
		t.Errorf("discovery returned %d entries for 1 peer — it is enumerating objects",
			counting.childReturn)
	}
	if counting.keysListed != 0 {
		t.Errorf("discovery performed a full listing of %d keys", counting.keysListed)
	}
}

// The bound is exclusive: the batch named by the cursor has already been
// applied and must not come back.
func TestListAfterIsExclusive(t *testing.T) {
	bucket, _ := newBucket(t)
	ctx := context.Background()
	for _, k := range []string{"p/a", "p/b", "p/c"} {
		if err := bucket.Put(ctx, k, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	got, err := bucket.List(ctx, "p/", "p/b")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "p/c" {
		t.Fatalf("List after p/b = %v, want [p/c]", got)
	}
	all, err := bucket.List(ctx, "p/", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("an empty bound should list everything, got %v", all)
	}
}
