package sync

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/shellcrumbs/shcr/internal/crypto"
	"github.com/shellcrumbs/shcr/internal/event"
	"github.com/shellcrumbs/shcr/internal/store"
)

const (
	// Events per uploaded object. One object per command would multiply storage
	// operations — and cost — by three orders of magnitude.
	maxEventsPerBatch = 500

	// Batch keys are timestamped to the millisecond so lexical order is
	// chronological order; the random suffix keeps two batches inside the same
	// millisecond from colliding.
	batchTimeLayout = "2006-01-02T15-04-05.000Z"
)

// Manifest is the small object each device keeps at a fixed key. Polling reads
// it instead of listing the batch folder: a GET is a Class B operation and
// roughly ten times cheaper than the Class A LIST it replaces, and the LIST only
// happens when the manifest shows something actually changed.
//
// It holds no command data. The hostname hint is a convenience for naming
// devices in the UI and is the one identifying field here, so it is optional.
type Manifest struct {
	LatestBatch  string `json:"latest_batch"`
	UpdatedAt    string `json:"updated_at"`
	HostnameHint string `json:"hostname_hint,omitempty"`
}

type Engine struct {
	Store    *store.Store
	Storage  Storage
	Key      crypto.Key
	DeviceID string
	Hostname string
	Logger   *log.Logger

	// KeyFunc supplies the encryption key on first use instead of at
	// construction. A daemon started by the service manager comes up before the
	// login keyring is unlocked — often long before, and on a headless machine
	// perhaps never — and deciding at startup that the key is missing would
	// disable sync silently for the whole life of the process.
	KeyFunc func() (crypto.Key, error)

	// ShareHostname controls whether the manifest carries a hostname hint. The
	// storage provider can see it, so it is the user's call.
	ShareHostname bool

	// Enabled is consulted before each cycle in Run. A nil value means always.
	// It exists so the loop can be started for a machine that has a backend
	// configured but sync switched off, and pick the switch up while running.
	Enabled func() bool

	// triggers carries the moments worth syncing on into the loop.
	triggers chan Trigger

	mu        sync.Mutex
	keyReady  bool
	keyLogged bool

	// cycle serialises whole sync cycles. Two at once would read the same
	// pending events and upload them as two batches, and both would derive a
	// batch key from the same manifest and then overwrite each other's — so the
	// second one published is the only one a peer ever learns about.
	cycle sync.Mutex
}

// key returns the resolved encryption key. Reading e.Key directly races with
// ensureKey writing it, which -race only catches if a test actually runs two
// cycles at once.
func (e *Engine) key() crypto.Key {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.Key
}

// ensureKey resolves the key, retrying on every attempt until it arrives.
func (e *Engine) ensureKey() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.keyReady || e.KeyFunc == nil {
		return nil
	}
	k, err := e.KeyFunc()
	if err != nil {
		// Say it once. Repeating it every interval would bury the log of a
		// machine whose keyring is simply never unlocked.
		if !e.keyLogged {
			e.keyLogged = true
			e.logf("sync is waiting for the encryption key: %v", err)
		}
		return err
	}
	e.Key = k
	e.keyReady = true
	if e.keyLogged {
		e.logf("encryption key is available; sync is live")
	}
	return nil
}

func (e *Engine) logf(format string, args ...any) {
	if e.Logger != nil {
		e.Logger.Printf(format, args...)
	}
}

func manifestKey(deviceID string) string { return "devices/" + deviceID + "/manifest.json" }
func batchPrefix(deviceID string) string { return "devices/" + deviceID + "/batches/" }

// newBatchKey names the next batch so that it always sorts after the previous
// one this device published.
//
// Readers track their position in a peer's stream as a single key and skip
// anything at or below it, so a key that sorts backwards is not merely
// out of order — it is never read again. Wall clocks do step backwards: an NTP
// correction, a virtual machine resumed from a snapshot, a laptop with a flat
// coin cell. When that happens the timestamp is taken from the last published
// key instead, which keeps the sequence monotonic without needing the clock to
// be right.
func (e *Engine) newBatchKey(now time.Time, previous string) (string, error) {
	var suffix [2]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	// Compare at the precision the key is written with. The previous key's
	// timestamp comes back millisecond-truncated, so comparing a nanosecond
	// `now` against it reports "later" for two pushes inside the same
	// millisecond — and then ordering falls to the random suffix, which is a
	// coin toss. A key that sorts below the reader's cursor is never read again.
	ts := now.UTC().Truncate(time.Millisecond)
	if prev, ok := batchKeyTime(previous); ok && !ts.After(prev) {
		ts = prev.Add(time.Millisecond)
	}
	return fmt.Sprintf("%s%s_%s.jsonl.enc",
		batchPrefix(e.DeviceID), ts.Format(batchTimeLayout), hex.EncodeToString(suffix[:])), nil
}

// batchKeyTime recovers the timestamp a batch key was built from.
func batchKeyTime(key string) (time.Time, bool) {
	if key == "" {
		return time.Time{}, false
	}
	base := key[strings.LastIndex(key, "/")+1:]
	stamp, _, ok := strings.Cut(base, "_")
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(batchTimeLayout, stamp)
	return t, err == nil
}

// SyncOnce runs a full cycle. Push happens first so a machine that is about to
// go offline gets its own work uploaded before spending time reading.
func (e *Engine) SyncOnce(ctx context.Context) (pushed, pulled int, err error) {
	// One cycle at a time: the loop's timer and the dashboard's button reach
	// this from different goroutines.
	e.cycle.Lock()
	defer e.cycle.Unlock()

	if err := e.ensureKey(); err != nil {
		return 0, 0, err
	}
	pushed, err = e.PushOnce(ctx)
	if err != nil {
		return pushed, 0, fmt.Errorf("push: %w", err)
	}
	pulled, err = e.PullOnce(ctx)
	if err != nil {
		return pushed, pulled, fmt.Errorf("pull: %w", err)
	}
	return pushed, pulled, nil
}

// PushOnce uploads everything this device has pending, as one encrypted batch
// per maxEventsPerBatch events.
//
// It drains rather than sending a single batch and stopping. With one batch per
// cycle and a thirty second floor between cycles, the ten thousand events an
// import produces would have taken over an hour and a half to reach the other
// machines, and a backlog built up faster than one batch per cycle would never
// clear at all.
func (e *Engine) PushOnce(ctx context.Context) (int, error) {
	total := 0
	for {
		n, err := e.pushBatch(ctx)
		if err != nil {
			return total, err
		}
		total += n
		// A short batch means the queue is empty; anything else and there may
		// be more waiting.
		if n < maxEventsPerBatch {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}

// pushBatch uploads at most maxEventsPerBatch pending events and reports how
// many went. Zero means there was nothing to send.
func (e *Engine) pushBatch(ctx context.Context) (int, error) {
	events, err := e.Store.UnsyncedEvents(e.DeviceID, maxEventsPerBatch)
	if err != nil {
		return 0, err
	}
	if len(events) == 0 {
		return 0, nil
	}

	var plain bytes.Buffer
	enc := json.NewEncoder(&plain)
	ids := make([]string, 0, len(events))
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			return 0, err
		}
		ids = append(ids, ev.EventID)
	}

	sealed, err := crypto.Encrypt(e.key(), plain.Bytes())
	if err != nil {
		return 0, err
	}

	// Read our own manifest first: it names the last key we published, which is
	// what keeps the next one sorting after it.
	m, err := e.loadManifest(ctx, e.DeviceID)
	if err != nil {
		return 0, err
	}
	key, err := e.newBatchKey(time.Now(), m.LatestBatch)
	if err != nil {
		return 0, err
	}

	// The batch goes up before the manifest that names it, so a peer can never
	// read a pointer to an object that is not there yet.
	if err := e.Storage.Put(ctx, key, sealed); err != nil {
		return 0, err
	}

	m.LatestBatch = key
	m.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	m.HostnameHint = ""
	if e.ShareHostname {
		m.HostnameHint = e.Hostname
	}
	mb, err := json.Marshal(m)
	if err != nil {
		return 0, err
	}
	if err := e.Storage.Put(ctx, manifestKey(e.DeviceID), mb); err != nil {
		return 0, err
	}

	// Marking synced last means a crash mid-push re-uploads these events in a
	// fresh batch rather than losing them. The duplicate is harmless: merging is
	// insert-by-event-id.
	if err := e.Store.MarkSynced(ids); err != nil {
		return 0, err
	}
	e.logf("pushed %d event(s) as %s", len(events), key)
	return len(events), nil
}

func (e *Engine) loadManifest(ctx context.Context, deviceID string) (Manifest, error) {
	b, err := e.Storage.Get(ctx, manifestKey(deviceID))
	if err != nil {
		if errors.Is(err, ErrNotExist) {
			return Manifest{}, nil
		}
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("manifest for %s is unreadable: %w", deviceID, err)
	}
	return m, nil
}

// Peers lists every device that has ever written to the bucket, excluding this
// one.
//
// A delimited listing, not a full one: enumerating every object just to read
// device ids out of the paths would cost the entire bucket on every poll, and
// grow with history for information that only changes when a machine is added.
func (e *Engine) Peers(ctx context.Context) ([]string, error) {
	ids, err := e.Storage.Children(ctx, "devices/")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, id := range ids {
		if id == "" || id == e.DeviceID {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// PullOnce reads whatever each peer has published since we last looked.
func (e *Engine) PullOnce(ctx context.Context) (int, error) {
	peers, err := e.Peers(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	var failures []error
	for _, peer := range peers {
		n, err := e.pullPeer(ctx, peer)
		total += n
		if err != nil {
			// One unreachable or corrupt peer must not stop the others.
			e.logf("peer %s: %v", peer, err)
			failures = append(failures, fmt.Errorf("peer %s: %w", peer, err))
			continue
		}
	}
	// Every peer failing is not a successful sync of nothing. It is what a wrong
	// key looks like — no batch decrypts, so all of them fail — and reporting
	// success there means `sync now` exits 0 and the loop never backs off.
	// A single bad peer among working ones stays a logged warning.
	if len(failures) > 0 && len(failures) == len(peers) {
		return total, errors.Join(failures...)
	}
	return total, nil
}

func (e *Engine) pullPeer(ctx context.Context, peer string) (int, error) {
	m, err := e.loadManifest(ctx, peer)
	if err != nil {
		return 0, err
	}
	cursor, err := e.Store.Cursor(peer)
	if err != nil {
		return 0, err
	}
	// The cheap check: nothing new since last time, so no LIST at all.
	if m.LatestBatch == "" || m.LatestBatch <= cursor.LastBatchKey {
		return 0, nil
	}

	// Ask only for what comes after where we stopped. Without the bound this
	// enumerates every batch the peer has ever written, on every sync that finds
	// anything new, for the life of the history.
	keys, err := e.Storage.List(ctx, batchPrefix(peer), cursor.LastBatchKey)
	if err != nil {
		return 0, err
	}

	applied := 0
	var failed error
	for _, key := range keys {
		// Defensive: a backend that ignored the bound must not make us reapply
		// and re-advance over batches already consumed.
		if key <= cursor.LastBatchKey {
			continue
		}
		n, err := e.applyBatch(ctx, key)
		if err != nil {
			// Stop at the first unreadable batch rather than skipping past it:
			// advancing the cursor over a batch we could not read would lose
			// those events permanently.
			failed = fmt.Errorf("batch %s: %w", key, err)
			break
		}
		applied += n
		cursor.LastBatchKey = key
	}

	cursor.PeerDeviceID = peer
	if m.HostnameHint != "" {
		cursor.HostnameHint = m.HostnameHint
	}
	// Only a clean pass counts as having synced this peer; the cursor itself is
	// saved either way. Keeping the progress made before a bad batch is what
	// stops every later sync from re-listing, re-fetching and re-decrypting the
	// batches that were fine, for as long as the bad one stays unreadable.
	if failed == nil {
		cursor.LastSyncedAt = time.Now().UnixMilli()
	}
	if err := e.Store.SaveCursor(cursor); err != nil {
		return applied, err
	}
	if failed != nil {
		return applied, failed
	}
	if applied > 0 {
		e.logf("pulled %d event(s) from %s", applied, peer)
	}
	return applied, nil
}

func (e *Engine) applyBatch(ctx context.Context, key string) (int, error) {
	sealed, err := e.Storage.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	plain, err := crypto.Decrypt(e.key(), sealed)
	if err != nil {
		return 0, err
	}

	sc := bufio.NewScanner(bytes.NewReader(plain))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	n := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev event.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return n, fmt.Errorf("malformed event in batch: %w", err)
		}
		inserted, err := e.Store.InsertRemoteEvent(ev)
		if err != nil {
			return n, err
		}
		if inserted {
			n++
		}
	}
	return n, sc.Err()
}
