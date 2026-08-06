package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
)

// fakeGCS is enough of the Cloud Storage JSON API to hold this backend to its
// side of the contract. The semantics that matter are copied deliberately:
// startOffset is inclusive, listings are lexicographic, and they page.
type fakeGCS struct {
	t        *testing.T
	bucket   string
	objects  map[string][]byte
	pageSize int
	requests int
}

func newFakeGCS(t *testing.T, bucket string) (*fakeGCS, *GCSStorage, *httptest.Server) {
	t.Helper()
	f := &fakeGCS{t: t, bucket: bucket, objects: map[string][]byte{}, pageSize: 1000}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, &GCSStorage{Bucket: bucket, client: srv.Client(), endpoint: srv.URL}, srv
}

func (f *fakeGCS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests++
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/upload/storage/v1/b/"):
		f.put(w, r)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/o"):
		f.list(w, r)
	case r.Method == http.MethodGet:
		f.get(w, r)
	default:
		http.Error(w, "unexpected", http.StatusBadRequest)
	}
}

func (f *fakeGCS) apiError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": msg},
	})
}

func (f *fakeGCS) put(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		f.apiError(w, http.StatusBadRequest, "no name")
		return
	}
	buf := make([]byte, r.ContentLength)
	if _, err := r.Body.Read(buf); err != nil && err.Error() != "EOF" {
		f.t.Logf("read body: %v", err)
	}
	f.objects[name] = buf
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"name": name})
}

func (f *fakeGCS) get(w http.ResponseWriter, r *http.Request) {
	// EscapedPath, not Path: Go has already decoded Path by the time a handler
	// sees it, so reading that cannot tell whether the name was escaped on the
	// wire — which is the thing that makes a key with slashes in it addressable
	// as a single segment at all.
	escaped := r.URL.EscapedPath()
	i := strings.Index(escaped, "/o/")
	raw := escaped[i+len("/o/"):]
	if strings.Contains(raw, "/") {
		f.t.Errorf("object name reached the server unescaped: %q", raw)
	}
	name, err := url.PathUnescape(raw)
	if err != nil {
		f.apiError(w, http.StatusBadRequest, "bad name")
		return
	}
	body, ok := f.objects[name]
	if !ok {
		f.apiError(w, http.StatusNotFound, fmt.Sprintf("No such object: %s/%s", f.bucket, name))
		return
	}
	_, _ = w.Write(body)
}

func (f *fakeGCS) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix, delim, start, token := q.Get("prefix"), q.Get("delimiter"), q.Get("startOffset"), q.Get("pageToken")

	var names []string
	for n := range f.objects {
		names = append(names, n)
	}
	sort.Strings(names)

	var items []map[string]string
	seen := map[string]bool{}
	var prefixes []string
	for _, n := range names {
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		// Inclusive, exactly as documented. A backend that forwards its
		// exclusive `after` straight through would re-read one batch forever.
		if start != "" && n < start {
			continue
		}
		if n <= token {
			continue
		}
		if delim != "" {
			if i := strings.Index(n[len(prefix):], delim); i >= 0 {
				p := n[:len(prefix)+i+len(delim)]
				if !seen[p] {
					seen[p] = true
					prefixes = append(prefixes, p)
				}
				continue
			}
		}
		items = append(items, map[string]string{"name": n})
		if len(items) >= f.pageSize {
			break
		}
	}

	out := map[string]any{}
	if len(items) > 0 {
		out["items"] = items
		if len(items) >= f.pageSize {
			out["nextPageToken"] = items[len(items)-1]["name"]
		}
	}
	if len(prefixes) > 0 {
		out["prefixes"] = prefixes
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// ---------------------------------------------------------------- conformance

// The engine is written against Storage and nothing else, so the two backends
// have to be genuinely interchangeable. Everything here runs against both: a
// difference between them is a bug in whichever one is not the directory, which
// is the one every other test in this package already exercises.
func TestBothBackendsBehaveTheSame(t *testing.T) {
	backends := map[string]func(*testing.T) Storage{
		"file": func(t *testing.T) Storage {
			s, err := NewFileStorage(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
		"gcs": func(t *testing.T) Storage {
			_, g, _ := newFakeGCS(t, "shellcrumbs")
			return g
		},
	}

	cases := []struct {
		name string
		run  func(*testing.T, Storage)
	}{
		{"a stored object comes back byte for byte", func(t *testing.T, s Storage) {
			ctx := context.Background()
			// Ciphertext: arbitrary bytes, including a NUL and a newline.
			want := []byte{0x00, 0xff, '\n', 0x7f, 'a', 0x00}
			if err := s.Put(ctx, "devices/dev-1/batch_001.jsonl.enc", want); err != nil {
				t.Fatal(err)
			}
			got, err := s.Get(ctx, "devices/dev-1/batch_001.jsonl.enc")
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Errorf("got %v, want %v", got, want)
			}
		}},

		{"a missing object is ErrNotExist, not some other error", func(t *testing.T, s Storage) {
			_, err := s.Get(context.Background(), "devices/dev-9/manifest.json")
			if !errors.Is(err, ErrNotExist) {
				t.Errorf("got %v, want ErrNotExist — the engine treats a new peer as one with no manifest", err)
			}
		}},

		{"overwriting replaces, because the manifest is rewritten every round", func(t *testing.T, s Storage) {
			ctx := context.Background()
			for _, v := range []string{"first", "second"} {
				if err := s.Put(ctx, "devices/dev-1/manifest.json", []byte(v)); err != nil {
					t.Fatal(err)
				}
			}
			got, err := s.Get(ctx, "devices/dev-1/manifest.json")
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "second" {
				t.Errorf("got %q", got)
			}
		}},

		{"list is ordered and strictly after the cursor", func(t *testing.T, s Storage) {
			ctx := context.Background()
			keys := []string{
				"devices/dev-1/b_001.enc",
				"devices/dev-1/b_002.enc",
				"devices/dev-1/b_003.enc",
			}
			for _, k := range keys {
				if err := s.Put(ctx, k, []byte("x")); err != nil {
					t.Fatal(err)
				}
			}
			all, err := s.List(ctx, "devices/dev-1/", "")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(all, ",") != strings.Join(keys, ",") {
				t.Errorf("got %v, want %v in order", all, keys)
			}
			// Strictly after: the cursor names the last batch already read, and
			// returning it again would replay it on every single sync round.
			rest, err := s.List(ctx, "devices/dev-1/", keys[1])
			if err != nil {
				t.Fatal(err)
			}
			if len(rest) != 1 || rest[0] != keys[2] {
				t.Errorf("got %v, want only %v", rest, keys[2:])
			}
		}},

		{"list stays inside its prefix", func(t *testing.T, s Storage) {
			ctx := context.Background()
			for _, k := range []string{"devices/dev-1/a.enc", "devices/dev-2/a.enc"} {
				if err := s.Put(ctx, k, []byte("x")); err != nil {
					t.Fatal(err)
				}
			}
			got, err := s.List(ctx, "devices/dev-1/", "")
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0] != "devices/dev-1/a.enc" {
				t.Errorf("got %v, want only dev-1's object", got)
			}
		}},

		{"children names the peers, not their objects", func(t *testing.T, s Storage) {
			ctx := context.Background()
			for _, k := range []string{
				"devices/dev-1/manifest.json",
				"devices/dev-1/b_001.enc",
				"devices/dev-2/manifest.json",
			} {
				if err := s.Put(ctx, k, []byte("x")); err != nil {
					t.Fatal(err)
				}
			}
			got, err := s.Children(ctx, "devices/")
			if err != nil {
				t.Fatal(err)
			}
			sort.Strings(got)
			if strings.Join(got, ",") != "dev-1,dev-2" {
				t.Errorf("got %v, want [dev-1 dev-2]", got)
			}
		}},

		{"children of nothing is empty, not an error", func(t *testing.T, s Storage) {
			got, err := s.Children(context.Background(), "devices/")
			if err != nil {
				t.Fatalf("a bucket with no peers yet is not an error: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("got %v", got)
			}
		}},
	}

	for backend, mk := range backends {
		for _, tc := range cases {
			t.Run(backend+"/"+tc.name, func(t *testing.T) { tc.run(t, mk(t)) })
		}
	}
}

// ---------------------------------------------------------------- gcs only

// A year of syncing holds more objects than one page returns. Stopping at the
// first page would hide history that is in the bucket from the machine pulling
// it, and it would look like nothing was wrong.
func TestListReadsEveryPage(t *testing.T) {
	f, g, _ := newFakeGCS(t, "shellcrumbs")
	f.pageSize = 3

	var want []string
	for i := range 10 {
		k := fmt.Sprintf("devices/dev-1/b_%03d.enc", i)
		if err := g.Put(context.Background(), k, []byte("x")); err != nil {
			t.Fatal(err)
		}
		want = append(want, k)
	}
	got, err := g.List(context.Background(), "devices/dev-1/", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %d of %d keys:\n %v", len(got), len(want), got)
	}
}

// A bucket may hold more than shcr, so everything can be nested under a folder.
// The caller must never see that folder: it hands in the keys it knows and gets
// the same ones back.
func TestABucketPrefixIsInvisibleToTheCaller(t *testing.T) {
	f, g, _ := newFakeGCS(t, "shellcrumbs")
	g.Prefix = normalisePrefix("/shcr/")
	ctx := context.Background()

	if err := g.Put(ctx, "devices/dev-1/b_001.enc", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.objects["shcr/devices/dev-1/b_001.enc"]; !ok {
		var names []string
		for n := range f.objects {
			names = append(names, n)
		}
		t.Fatalf("not stored under the prefix: %v", names)
	}
	got, err := g.List(ctx, "devices/dev-1/", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "devices/dev-1/b_001.enc" {
		t.Errorf("caller saw %v, want its own key back", got)
	}
	kids, err := g.Children(ctx, "devices/")
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 1 || kids[0] != "dev-1" {
		t.Errorf("children = %v", kids)
	}
	if _, err := g.Get(ctx, "devices/dev-1/b_001.enc"); err != nil {
		t.Errorf("get through the prefix: %v", err)
	}
}

// The two failures anyone will actually hit are a bucket that is not there and
// a service account without permission. Both are a bare status code unless the
// message is carried out.
func TestAnApiErrorSaysWhatWentWrong(t *testing.T) {
	f, g, _ := newFakeGCS(t, "shellcrumbs")
	f.objects["devices/dev-1/x.enc"] = []byte("x")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.apiError(w, http.StatusForbidden,
			"shcr@example.iam.gserviceaccount.com does not have storage.objects.list access")
	}))
	defer srv.Close()
	g.endpoint = srv.URL

	_, err := g.List(context.Background(), "devices/", "")
	if err == nil {
		t.Fatal("no error")
	}
	if !strings.Contains(err.Error(), "does not have storage.objects.list access") {
		t.Errorf("the error hides the reason: %v", err)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("the error hides the status: %v", err)
	}
}

func TestBucketNameIsNotAURL(t *testing.T) {
	for _, bad := range []string{"gs://shellcrumbs", "shellcrumbs/shcr", "https://x"} {
		if _, err := NewGCSStorage(context.Background(), bad, ""); err == nil {
			t.Errorf("%q was accepted as a bucket name", bad)
		}
	}
}

func TestNormalisePrefix(t *testing.T) {
	for in, want := range map[string]string{
		"":            "",
		"/":           "",
		"shcr":        "shcr/",
		"/shcr/":      "shcr/",
		"a/b":         "a/b/",
		"///a/b/////": "a/b/",
	} {
		if got := normalisePrefix(in); got != want {
			t.Errorf("normalisePrefix(%q) = %q, want %q", in, got, want)
		}
	}
}
