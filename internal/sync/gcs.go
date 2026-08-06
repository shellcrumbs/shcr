package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
)

// gcsScope is write access to objects, and nothing else. Not the full-control
// scope: this backend never touches bucket configuration, IAM or lifecycle, and
// a token that cannot do those things cannot be made to do them by a bug here.
const gcsScope = "https://www.googleapis.com/auth/devstorage.read_write"

const gcsEndpoint = "https://storage.googleapis.com"

// GCSStorage keeps the bucket in Google Cloud Storage.
//
// It speaks the JSON API over net/http rather than using cloud.google.com/go/
// storage. That library brings 457 packages and roughly 30MB with it, which
// would nearly triple a binary that is 17MB whole, to reach four calls that are
// one HTTP request each. The credentials are the part worth borrowing, so
// golang.org/x/oauth2/google resolves those and nothing else is imported.
//
// Objects are already encrypted before they arrive here. Google sees ciphertext,
// object sizes and timestamps — see the README on what the storage provider can
// still infer.
type GCSStorage struct {
	// Bucket is the bucket name, without a scheme or path.
	Bucket string
	// Prefix optionally nests everything under a folder, so one bucket can hold
	// more than shcr. Empty means the bucket root.
	Prefix string

	client *http.Client
	// endpoint is the API root. Only tests set it.
	endpoint string
	// sleep is time.Sleep, indirected so a retry test does not have to spend the
	// backoff it is asserting.
	sleep func(time.Duration)
}

// retryAttempts bounds how long a single call will keep trying. Cloud Storage
// caps mutations of one object at roughly one per second, and the manifest is
// rewritten once per batch — so a first sync with a backlog walks straight into
// it. Everything here is idempotent (a batch key is unique, the manifest is
// last-writer-wins) so a retry cannot do damage a first attempt would not.
const retryAttempts = 6

// retryBase is the first backoff. It doubles per attempt, so six attempts span
// about eight seconds — comfortably past a per-second limit, and short enough
// that a genuinely broken bucket still fails inside one sync round.
const retryBase = 250 * time.Millisecond

// NewGCSStorage resolves Application Default Credentials and returns a backend
// for the bucket.
//
// ADC is deliberate: it picks up GOOGLE_APPLICATION_CREDENTIALS, a service
// account key, `gcloud auth application-default login`, or the metadata server
// on a GCE instance, without shcr having to hold a credential of its own or
// invent a place to keep one.
func NewGCSStorage(ctx context.Context, bucket, prefix string) (*GCSStorage, error) {
	if bucket == "" {
		return nil, fmt.Errorf("gcs: no bucket configured")
	}
	if strings.ContainsAny(bucket, "/:") {
		return nil, fmt.Errorf("gcs: %q is a bucket name, not a URL", bucket)
	}
	client, err := google.DefaultClient(ctx, gcsScope)
	if err != nil {
		return nil, fmt.Errorf("gcs: no credentials (try `gcloud auth application-default login`, "+
			"or set GOOGLE_APPLICATION_CREDENTIALS to a service account key): %w", err)
	}
	return &GCSStorage{Bucket: bucket, Prefix: normalisePrefix(prefix), client: client}, nil
}

// normalisePrefix makes a configured prefix a folder: no leading slash, exactly
// one trailing slash, or empty.
func normalisePrefix(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

func (g *GCSStorage) root() string {
	if g.endpoint != "" {
		return g.endpoint
	}
	return gcsEndpoint
}

func (g *GCSStorage) http() *http.Client {
	if g.client != nil {
		return g.client
	}
	return http.DefaultClient
}

// send performs a request, retrying the failures an object store is expected to
// produce: a rate limit, and the transient 5xx that any large distributed system
// returns occasionally. The request is rebuilt per attempt so a body can be sent
// again.
//
// Anything else — a missing object, a permission error, a bad bucket — comes
// straight back. Retrying those only turns a clear error into a slow one.
func (g *GCSStorage) send(ctx context.Context, build func() (*http.Request, error)) (*http.Response, error) {
	nap := g.sleep
	if nap == nil {
		nap = time.Sleep
	}
	var lastErr error
	for attempt := range retryAttempts {
		if attempt > 0 {
			wait := retryBase << (attempt - 1)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
			nap(wait)
		}
		req, err := build()
		if err != nil {
			return nil, err
		}
		resp, err := g.http().Do(req)
		if err != nil {
			// A connection that failed mid-flight is worth another go; a
			// cancelled context is not.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		if !retryable(resp.StatusCode) {
			return resp, nil
		}
		// The body has to be drained and closed or the connection leaks.
		lastErr = gcsError(resp, "retryable")
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
	}
	return nil, fmt.Errorf("gave up after %d attempts: %w", retryAttempts, lastErr)
}

// retryable is the set worth trying again: the rate limit on mutating one
// object, and the transient server-side failures.
func retryable(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// name maps a caller's key to the object name in the bucket.
func (g *GCSStorage) name(key string) string { return g.Prefix + key }

// key maps an object name back to what the caller asked about. Callers get back
// the keys they use; the bucket prefix is this type's business alone.
func (g *GCSStorage) key(name string) string { return strings.TrimPrefix(name, g.Prefix) }

func (g *GCSStorage) Get(ctx context.Context, key string) ([]byte, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	// PathEscape, so a key's slashes become %2F: the object name is one path
	// segment to the API, not a path.
	u := fmt.Sprintf("%s/storage/v1/b/%s/o/%s?alt=media",
		g.root(), url.PathEscape(g.Bucket), url.PathEscape(g.name(key)))

	resp, err := g.send(ctx, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	})
	if err != nil {
		return nil, fmt.Errorf("gcs get %s: %w", key, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s: %w", key, ErrNotExist)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, gcsError(resp, "get "+key)
	}
	return io.ReadAll(resp.Body)
}

func (g *GCSStorage) Put(ctx context.Context, key string, data []byte) error {
	if err := checkKey(key); err != nil {
		return err
	}
	q := url.Values{"uploadType": {"media"}, "name": {g.name(key)}}
	u := fmt.Sprintf("%s/upload/storage/v1/b/%s/o?%s", g.root(), url.PathEscape(g.Bucket), q.Encode())

	resp, err := g.send(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		req.ContentLength = int64(len(data))
		return req, nil
	})
	if err != nil {
		return fmt.Errorf("gcs put %s: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return gcsError(resp, "put "+key)
	}
	// Drain, so the connection can be reused for the next batch in this round.
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (g *GCSStorage) List(ctx context.Context, prefix, after string) ([]string, error) {
	q := url.Values{"prefix": {g.name(prefix)}}
	if after != "" {
		// startOffset is inclusive and this call is not, so the exact match is
		// dropped below. Sending it anyway is what keeps the request
		// proportional to what is new: without it the server enumerates every
		// object ever written on every poll.
		q.Set("startOffset", g.name(after))
	}
	var out []string
	err := g.eachPage(ctx, q, func(p gcsListPage) {
		for _, it := range p.Items {
			if k := g.key(it.Name); k > after {
				out = append(out, k)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	// The API returns names in lexicographic order and pages preserve it, so
	// what arrives is already sorted.
	return out, nil
}

func (g *GCSStorage) Children(ctx context.Context, prefix string) ([]string, error) {
	full := g.name(prefix)
	q := url.Values{"prefix": {full}, "delimiter": {"/"}}
	var out []string
	err := g.eachPage(ctx, q, func(p gcsListPage) {
		for _, pfx := range p.Prefixes {
			// The API answers with whole prefixes; callers want the child's own
			// name, which is what the directory backend returns.
			if name := strings.TrimSuffix(strings.TrimPrefix(pfx, full), "/"); name != "" {
				out = append(out, name)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type gcsListPage struct {
	Items         []struct{ Name string } `json:"items"`
	Prefixes      []string                `json:"prefixes"`
	NextPageToken string                  `json:"nextPageToken"`
}

// eachPage walks a paginated listing. A bucket that has been syncing for a year
// holds more objects than one page returns, and stopping at the first page
// would silently hide the rest — which for a listing means history that is
// present in the bucket and invisible to the machine pulling it.
func (g *GCSStorage) eachPage(ctx context.Context, q url.Values, fn func(gcsListPage)) error {
	for {
		u := fmt.Sprintf("%s/storage/v1/b/%s/o?%s", g.root(), url.PathEscape(g.Bucket), q.Encode())
		resp, err := g.send(ctx, func() (*http.Request, error) {
			return http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		})
		if err != nil {
			return fmt.Errorf("gcs list %s: %w", q.Get("prefix"), err)
		}
		var page gcsListPage
		if resp.StatusCode != http.StatusOK {
			err := gcsError(resp, "list "+q.Get("prefix"))
			resp.Body.Close()
			return err
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("gcs list %s: %w", q.Get("prefix"), err)
		}
		fn(page)
		if page.NextPageToken == "" {
			return nil
		}
		q.Set("pageToken", page.NextPageToken)
	}
}

// checkKey rejects what the directory backend also rejects, so a key that is
// wrong is wrong on every backend rather than only on the one nobody tested.
func checkKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") {
		return fmt.Errorf("invalid object key %q", key)
	}
	return nil
}

// gcsError turns an error response into something that says what to fix. The
// common ones here are a bucket that does not exist and a service account
// without objectAdmin, and both look identical without the body.
func gcsError(resp *http.Response, what string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &parsed) == nil && parsed.Error.Message != "" {
		msg = parsed.Error.Message
	}
	return fmt.Errorf("gcs %s: %s: %s", what, resp.Status, msg)
}
