package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shellcrumbs/shcr/internal/event"
	"github.com/shellcrumbs/shcr/internal/store"
)

func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	srv, err := New(st, "dev-local", "this-host", nil)
	if err != nil {
		t.Fatal(err)
	}
	return srv, st
}

func record(t *testing.T, st *store.Store, id, text, status string, exit int, at int64) {
	t.Helper()
	start, err := event.New(id, "dev-local", event.TypeStart, event.StartPayload{
		Command: text, Hostname: "this-host", SessionID: "sess-1",
		Cwd: "/home/u/app", Shell: "bash", StartTime: at, PGID: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	start.CreatedAt = at
	if _, err := st.AppendEvent(start); err != nil {
		t.Fatal(err)
	}
	if status == store.StatusRunning {
		return
	}
	end, err := event.New(id, "dev-local", event.TypeEnd, event.EndPayload{EndTime: at + 500, ExitCode: exit})
	if err != nil {
		t.Fatal(err)
	}
	end.CreatedAt = at + 500
	if _, err := st.AppendEvent(end); err != nil {
		t.Fatal(err)
	}
}

func do(t *testing.T, srv *Server, method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

func withToken(srv *Server, path string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "token=" + srv.Token()
}

// Localhost is not a security boundary on a shared machine: any other user's
// process can reach a loopback port.
func TestAPIRequiresTheToken(t *testing.T) {
	srv, st := newTestServer(t)
	record(t, st, "c1", "npm run build", store.StatusCompleted, 0, 1000)

	for _, path := range []string{
		"/api/commands", "/api/commands/c1", "/api/stats",
		"/api/devices", "/api/settings", "/api/events",
	} {
		if got := do(t, srv, "GET", path, "").Code; got != http.StatusUnauthorized {
			t.Errorf("GET %s without a token returned %d, want 401", path, got)
		}
	}
	if got := do(t, srv, "POST", "/api/commands/c1/redact", "").Code; got != http.StatusUnauthorized {
		t.Errorf("redact without a token returned %d, want 401", got)
	}
	// A wrong token is no better than none.
	if got := do(t, srv, "GET", "/api/commands?token=guess", "").Code; got != http.StatusUnauthorized {
		t.Errorf("wrong token returned %d, want 401", got)
	}
	// And the right one works, by header as well as by query.
	if got := do(t, srv, "GET", withToken(srv, "/api/commands"), "").Code; got != http.StatusOK {
		t.Errorf("valid token returned %d, want 200", got)
	}
	r := httptest.NewRequest("GET", "/api/commands", nil)
	r.Header.Set("Authorization", "Bearer "+srv.Token())
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("bearer header returned %d, want 200", w.Code)
	}
}

func TestTokenIsFreshPerServer(t *testing.T) {
	a, _ := newTestServer(t)
	b, _ := newTestServer(t)
	if a.Token() == b.Token() {
		t.Fatal("two servers produced the same token")
	}
	if len(a.Token()) < 24 {
		t.Fatalf("token looks too short to be worth having: %q", a.Token())
	}
}

func TestListenBindsLoopbackOnly(t *testing.T) {
	srv, _ := newTestServer(t)
	ln, err := srv.Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	host, _, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("listening on %s; the dashboard must never be reachable off this machine", host)
	}
	if !strings.Contains(srv.URL(ln), "token=") {
		t.Error("the printed URL should carry the token")
	}
}

func TestCommandsEndpointFiltersAndPaginates(t *testing.T) {
	srv, st := newTestServer(t)
	record(t, st, "c1", "npm run build:prod", store.StatusFailed, 1, 1000)
	record(t, st, "c2", "git push", store.StatusCompleted, 0, 2000)
	record(t, st, "c3", "npm test", store.StatusRunning, 0, 3000)

	cases := map[string]int{
		"/api/commands":                3,
		"/api/commands?q=npm":          2,
		"/api/commands?status=failed":  1,
		"/api/commands?status=running": 1,
		"/api/commands?limit=1":        1,
		"/api/commands?q=terraform":    0,
	}
	for path, want := range cases {
		w := do(t, srv, "GET", withToken(srv, path), "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		var got struct {
			Commands []store.Command `json:"commands"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(got.Commands) != want {
			t.Errorf("%s returned %d commands, want %d", path, len(got.Commands), want)
		}
	}

	// An empty result must be [] and not null, so the frontend can iterate it.
	w := do(t, srv, "GET", withToken(srv, "/api/commands?q=terraform"), "")
	if !strings.Contains(w.Body.String(), `"commands":[]`) {
		t.Errorf("empty result should serialise as [], got %s", w.Body.String())
	}
}

func TestCommandDetailIncludesSessionContext(t *testing.T) {
	srv, st := newTestServer(t)
	record(t, st, "c1", "cd ~/app", store.StatusCompleted, 0, 1000)
	record(t, st, "c2", "git pull", store.StatusCompleted, 0, 2000)
	record(t, st, "c3", "npm run build", store.StatusFailed, 1, 3000)

	w := do(t, srv, "GET", withToken(srv, "/api/commands/c3"), "")
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var got struct {
		Command       store.Command   `json:"command"`
		SessionBefore []store.Command `json:"session_before"`
		OutputCapture bool            `json:"output_captured"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Command.Command != "npm run build" {
		t.Errorf("wrong command returned: %q", got.Command.Command)
	}
	if len(got.SessionBefore) != 2 {
		t.Errorf("expected the two preceding commands, got %d", len(got.SessionBefore))
	}
	// The honest empty state, rather than a panel that looks broken.
	if got.OutputCapture {
		t.Error("output capture is not implemented and must not claim to be")
	}

	if code := do(t, srv, "GET", withToken(srv, "/api/commands/nope"), "").Code; code != http.StatusNotFound {
		t.Errorf("unknown id returned %d, want 404", code)
	}
}

func TestRedactThroughTheAPIReplacesTextEverywhere(t *testing.T) {
	srv, st := newTestServer(t)
	record(t, st, "c1", "curl -H 'X-Key: leaked-value-here'", store.StatusCompleted, 0, 1000)

	w := do(t, srv, "POST", withToken(srv, "/api/commands/c1/redact"), "")
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "leaked-value-here") {
		t.Error("the response echoed the secret back")
	}

	c, err := st.CommandByID("c1")
	if err != nil || c == nil {
		t.Fatal(err)
	}
	if c.Command != event.RedactedMarker {
		t.Errorf("stored command is %q, want the redaction marker", c.Command)
	}
	// It must be gone from search too, not merely hidden from one view.
	hits, err := st.QueryCommands(store.Filter{Text: "leaked"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Error("redacted text is still reachable through search")
	}
	// And it must have been recorded as an event, which is what syncs it.
	var n int
	if err := st.DB().QueryRow(
		`SELECT count(*) FROM events WHERE command_id='c1' AND type='redact'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected one redact event so the change propagates, got %d", n)
	}
}

func TestStatsSummary(t *testing.T) {
	srv, st := newTestServer(t)
	now := time.Now()
	record(t, st, "c1", "a", store.StatusCompleted, 0, now.Add(-2*time.Hour).UnixMilli())
	record(t, st, "c2", "b", store.StatusFailed, 1, now.Add(-time.Hour).UnixMilli())
	record(t, st, "c3", "c", store.StatusRunning, 0, now.UnixMilli())

	w := do(t, srv, "GET", withToken(srv, "/api/stats"), "")
	var got store.Stats
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Total != 3 || got.Failed != 1 || got.Running != 1 || got.Completed != 1 {
		t.Errorf("counts wrong: %+v", got)
	}
	// A rhythm with gaps in it is the point, so quiet hours are present as zeroes.
	if len(got.Hourly) != 24 {
		t.Fatalf("histogram has %d buckets, want 24", len(got.Hourly))
	}
	total := 0
	for _, h := range got.Hourly {
		total += h.Count
	}
	if total != 3 {
		t.Errorf("histogram accounts for %d commands, want 3", total)
	}
}

func TestDevicesListsThisMachine(t *testing.T) {
	srv, st := newTestServer(t)
	record(t, st, "c1", "a", store.StatusCompleted, 0, 1000)
	if err := st.SaveCursor(store.Cursor{
		PeerDeviceID: "dev-remote", HostnameHint: "build-server", LastSyncedAt: 42,
	}); err != nil {
		t.Fatal(err)
	}

	w := do(t, srv, "GET", withToken(srv, "/api/devices"), "")
	var got struct {
		Devices []Device `json:"devices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Devices) != 2 {
		t.Fatalf("expected this machine plus one peer, got %d", len(got.Devices))
	}
	if !got.Devices[0].IsThisDevice || got.Devices[0].Commands != 1 {
		t.Errorf("this machine reported wrongly: %+v", got.Devices[0])
	}
	if got.Devices[1].Hostname != "build-server" {
		t.Errorf("peer hostname missing: %+v", got.Devices[1])
	}
}

// The browser must never be handed the encryption key.
func TestSettingsNeverExposeTheKey(t *testing.T) {
	srv, _ := newTestServer(t)
	body := do(t, srv, "GET", withToken(srv, "/api/settings"), "").Body.String()
	for _, forbidden := range []string{"key", "phrase", "mnemonic", "secret"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Errorf("settings response mentions %q: %s", forbidden, body)
		}
	}
}

// sync_available described how the server was wired at startup rather than what
// the configuration says now, so the dashboard would offer a working sync
// button on a machine with no backend at all.
func TestSyncAvailabilityFollowsTheConfigurationNotTheWiring(t *testing.T) {
	srv, _ := newTestServer(t)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	srv.Sync = func(context.Context) (int, int, error) { return 0, 0, nil }

	var got struct {
		SyncAvailable bool `json:"sync_available"`
		SyncEnabled   bool `json:"sync_enabled"`
	}
	body := do(t, srv, "GET", withToken(srv, "/api/settings"), "").Body.Bytes()
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.SyncAvailable {
		t.Errorf("sync reported available with no backend configured: %s", body)
	}
}

// The table lists executions, so without this a command run many times looks
// exactly like one run once.
func TestCommandDetailSaysHowOftenItHasRun(t *testing.T) {
	srv, st := newTestServer(t)
	at := int64(1_700_000_000_000)
	for i := range 5 {
		record(t, st, fmt.Sprintf("r%d", i), "npm run build", store.StatusCompleted, 0,
			at+int64(i)*60_000)
	}
	if _, _, err := st.RefreshCommandStats(0); err != nil {
		t.Fatal(err)
	}

	body := do(t, srv, "GET", withToken(srv, "/api/commands/r4"), "").Body.Bytes()
	var got struct {
		Usage struct {
			Runs      int    `json:"runs"`
			Succeeded int    `json:"succeeded"`
			Summary   string `json:"summary"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Usage.Runs != 5 || got.Usage.Succeeded != 5 {
		t.Errorf("usage = %+v, want 5 runs all succeeded", got.Usage)
	}
	if got.Usage.Summary != "ran 5× · 5 ok" {
		t.Errorf("summary = %q", got.Usage.Summary)
	}
}

func TestSyncEndpointReportsWhenUnavailable(t *testing.T) {
	srv, _ := newTestServer(t)
	if code := do(t, srv, "POST", withToken(srv, "/api/sync"), "").Code; code != http.StatusPreconditionFailed {
		t.Errorf("unconfigured sync returned %d, want 412", code)
	}

	srv.Sync = func(context.Context) (int, int, error) { return 3, 4, nil }
	w := do(t, srv, "POST", withToken(srv, "/api/sync"), "")
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var got map[string]int
	json.Unmarshal(w.Body.Bytes(), &got)
	if got["pushed"] != 3 || got["pulled"] != 4 {
		t.Errorf("sync result not reported: %v", got)
	}

	srv.Sync = func(context.Context) (int, int, error) { return 0, 0, fmt.Errorf("bucket unreachable") }
	w = do(t, srv, "POST", withToken(srv, "/api/sync"), "")
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "bucket unreachable") {
		t.Errorf("sync failure not surfaced: %d %s", w.Code, w.Body.String())
	}
}

func TestStaticPageIsServedWithoutAToken(t *testing.T) {
	srv, _ := newTestServer(t)
	w := do(t, srv, "GET", "/", "")
	if w.Code != http.StatusOK {
		t.Fatalf("index returned %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "shellcrumbs") {
		t.Error("index does not look like the dashboard")
	}
	// It carries no data, which is why it needs no token.
	if strings.Contains(w.Body.String(), srv.Token()) {
		t.Error("the static page must not embed the token")
	}
	if got := w.Header().Get("Content-Security-Policy"); got == "" {
		t.Error("no CSP on the dashboard")
	}
	if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options is %q, want DENY", got)
	}
}

// End to end over a real socket, because SSE cannot be exercised through
// httptest.ResponseRecorder — it never flushes.
func TestEventStreamPushesNewCommands(t *testing.T) {
	srv, st := newTestServer(t)
	ln, err := srv.Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx, ln)
	base := "http://" + ln.Addr().String()

	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/events?token="+srv.Token(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type is %q", ct)
	}

	lines := make(chan string, 32)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
	}()

	// Give the broker a moment to register the subscriber before writing.
	time.Sleep(1500 * time.Millisecond)
	record(t, st, "live-1", "echo streamed", store.StatusCompleted, 0, time.Now().UnixMilli())

	deadline := time.After(8 * time.Second)
	for {
		select {
		case line := <-lines:
			if strings.HasPrefix(line, "data: ") && strings.Contains(line, "echo streamed") {
				return // the change reached the browser
			}
		case <-deadline:
			t.Fatal("the new command never arrived on the event stream")
		}
	}
}

func TestEventStreamRejectsAnUnauthenticatedReader(t *testing.T) {
	srv, _ := newTestServer(t)
	ln, err := srv.Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx, ln)

	resp, err := http.Get("http://" + ln.Addr().String() + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated stream returned %d, want 401", resp.StatusCode)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
}

func TestHostsEndpoint(t *testing.T) {
	srv, st := newTestServer(t)
	record(t, st, "c1", "a", store.StatusCompleted, 0, 1000)
	w := do(t, srv, "GET", withToken(srv, "/api/hosts"), "")
	var got struct {
		Hosts []string `json:"hosts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Hosts) != 1 || got.Hosts[0] != "this-host" {
		t.Fatalf("hosts = %v, want [this-host]", got.Hosts)
	}
}

// The timeline needs commands on both sides of the selected one, or the
// selected command always sits at the bottom looking like the latest thing that
// happened.
func TestDetailTimelineSpansBothSides(t *testing.T) {
	srv, st := newTestServer(t)
	record(t, st, "c1", "cd ~/app", store.StatusCompleted, 0, 1000)
	record(t, st, "c2", "git pull", store.StatusCompleted, 0, 2000)
	record(t, st, "c3", "make", store.StatusFailed, 2, 3000)
	record(t, st, "c4", "make clean", store.StatusCompleted, 0, 4000)

	w := do(t, srv, "GET", withToken(srv, "/api/commands/c2"), "")
	var got struct {
		Before []store.Command `json:"session_before"`
		After  []store.Command `json:"session_after"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Before) != 1 || got.Before[0].ID != "c1" {
		t.Errorf("before = %+v, want just c1", got.Before)
	}
	if len(got.After) != 2 || got.After[0].ID != "c3" {
		t.Errorf("after = %+v, want c3 then c4 in order", got.After)
	}
}

// "3rd run today" is only meaningful for commands actually repeated today.
func TestRepeatCountsOnlyCoverRepeats(t *testing.T) {
	srv, st := newTestServer(t)
	now := time.Now()
	record(t, st, "r1", "make build", store.StatusCompleted, 0, now.Add(-3*time.Hour).UnixMilli())
	record(t, st, "r2", "make build", store.StatusCompleted, 0, now.Add(-2*time.Hour).UnixMilli())
	record(t, st, "r3", "make build", store.StatusFailed, 1, now.Add(-time.Hour).UnixMilli())
	record(t, st, "once", "git status", store.StatusCompleted, 0, now.Add(-time.Hour).UnixMilli())

	w := do(t, srv, "GET", withToken(srv, "/api/commands"), "")
	var got struct {
		Runs map[string]store.RunInfo `json:"runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Runs["once"]; ok {
		t.Error("a command run once should carry no repeat info")
	}
	if got.Runs["r1"].Ordinal != 1 || got.Runs["r3"].Ordinal != 3 || got.Runs["r3"].Total != 3 {
		t.Errorf("ordinals wrong: %+v", got.Runs)
	}
}

// The page shortens paths under this machine's home; it can only do that if the
// server tells it where home is.
func TestDevicesCarryTheLocalHomeDirectory(t *testing.T) {
	srv, _ := newTestServer(t)
	w := do(t, srv, "GET", withToken(srv, "/api/devices"), "")
	var got struct {
		Home string `json:"home"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Home == "" {
		t.Error("no home directory supplied, so the dashboard cannot shorten paths")
	}
}

// Everything the page needs must come from this binary; a dashboard that phones
// out to a CDN defeats the point of a local, private history tool.
func TestPageMakesNoExternalRequests(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, asset := range []string{"/", "/app.css", "/app.js"} {
		w := do(t, srv, "GET", asset, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s returned %d", asset, w.Code)
		}
		body := w.Body.String()
		for _, marker := range []string{"http://", "https://", "//fonts.", "cdn."} {
			if strings.Contains(body, marker) && !strings.Contains(body, "www.w3.org") {
				t.Errorf("%s references something external (%q)", asset, marker)
			}
		}
	}
	// And the fonts really are served from here.
	for _, f := range []string{"/fonts/jetbrains-mono-latin.woff2", "/fonts/ibm-plex-sans-latin.woff2"} {
		w := do(t, srv, "GET", f, "")
		if w.Code != http.StatusOK || w.Body.Len() < 1000 {
			t.Errorf("%s returned %d, %d bytes", f, w.Code, w.Body.Len())
		}
	}
}

// The recovery phrase is not served to the browser, by decision. If that ever
// changes it should be a deliberate act, not a drift.
func TestNoEndpointServesTheRecoveryPhrase(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, path := range []string{"/api/settings", "/api/devices", "/api/stats"} {
		body := strings.ToLower(do(t, srv, "GET", withToken(srv, path), "").Body.String())
		for _, word := range []string{"phrase", "mnemonic", "recovery"} {
			if strings.Contains(body, word) {
				t.Errorf("%s mentions %q", path, word)
			}
		}
	}
	// There is no key route at all.
	for _, path := range []string{"/api/key", "/api/phrase", "/api/recovery"} {
		if code := do(t, srv, "GET", withToken(srv, path), "").Code; code == http.StatusOK {
			t.Errorf("%s unexpectedly exists", path)
		}
	}
}

// A strict CSP is what keeps an injected command string from becoming script.
func TestContentSecurityPolicyHasNoInlineEscapeHatch(t *testing.T) {
	srv, _ := newTestServer(t)
	csp := do(t, srv, "GET", "/", "").Header().Get("Content-Security-Policy")
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("CSP allows inline code: %s", csp)
	}
	for _, directive := range []string{"default-src 'none'", "script-src 'self'", "font-src 'self'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP missing %q: %s", directive, csp)
		}
	}
}

// The activity chart once shared its modifier class with the table's
// empty-state block, so every bar silently inherited 40px of padding and the
// chart showed steady activity on an empty database. The DOM and the script
// were both correct; only the rendering was wrong. This checks the two files
// still agree on the name.
func TestChartModifierClassDoesNotCollide(t *testing.T) {
	js, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	css, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(js), `"bar" + (silent ? " empty"`) ||
		strings.Contains(string(js), `" empty"`) {
		t.Error(`the chart must not use "empty" as a bar modifier; .empty is the table's empty-state block`)
	}
	if !strings.Contains(string(js), `" silent"`) {
		t.Error("the chart no longer marks silent hours")
	}
	for _, rule := range []string{".bar.silent", ".bar:not(.silent)"} {
		if !strings.Contains(string(css), rule) {
			t.Errorf("stylesheet is missing %s, so silent hours will not be distinguishable", rule)
		}
	}
}

// A silent hour and an hour with work in it must not look the same.
func TestSilentAndActiveHoursHaveDifferentFloors(t *testing.T) {
	css, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	silent := regexp.MustCompile(`\.bar\.silent[^{]*\{[^}]*min-height:\s*(\d+)px`).FindStringSubmatch(string(css))
	active := regexp.MustCompile(`\.bar:not\(\.silent\)\s*\{[^}]*min-height:\s*(\d+)px`).FindStringSubmatch(string(css))
	if silent == nil || active == nil {
		t.Fatalf("could not find both floors: silent=%v active=%v", silent, active)
	}
	s, _ := strconv.Atoi(silent[1])
	a, _ := strconv.Atoi(active[1])
	if a < s*2 {
		t.Errorf("an active hour floors at %dpx against a silent %dpx — too close to tell apart", a, s)
	}
}
