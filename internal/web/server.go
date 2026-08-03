// Package web serves the local dashboard.
//
// Two rules shape the security posture. The listener binds 127.0.0.1 and never
// 0.0.0.0, so nothing on the network can reach it. And every API route requires
// a token generated fresh at startup, because "it's only localhost" is not true
// on a shared machine: any other user's process can connect to a loopback port.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"time"

	"github.com/shellcrumbs/shcr/internal/store"
)

//go:embed static
var staticFS embed.FS

// SyncFunc triggers a sync round. Nil when sync is not configured, which the
// dashboard reports rather than hiding.
type SyncFunc func(ctx context.Context) (pushed, pulled int, err error)

type Server struct {
	Store    *store.Store
	DeviceID string
	Hostname string
	Logger   *log.Logger
	Sync     SyncFunc

	token  string
	broker *broker
}

func New(st *store.Store, deviceID, hostname string, logger *log.Logger) (*Server, error) {
	tok, err := newToken()
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Server{
		Store: st, DeviceID: deviceID, Hostname: hostname,
		Logger: logger, token: tok, broker: newBroker(st, logger),
	}, nil
}

// Token is the secret embedded in the printed URL.
func (s *Server) Token() string { return s.token }

func newToken() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// Handler builds the router. Static assets are served without a token — they
// carry no data — while everything under /api does require one.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/commands", s.auth(s.handleCommands))
	mux.HandleFunc("GET /api/commands/{id}", s.auth(s.handleCommand))
	mux.HandleFunc("POST /api/commands/{id}/redact", s.auth(s.handleRedact))
	mux.HandleFunc("GET /api/stats", s.auth(s.handleStats))
	mux.HandleFunc("GET /api/hosts", s.auth(s.handleHosts))
	mux.HandleFunc("GET /api/devices", s.auth(s.handleDevices))
	mux.HandleFunc("GET /api/settings", s.auth(s.handleGetSettings))
	mux.HandleFunc("PATCH /api/settings", s.auth(s.handlePatchSettings))
	mux.HandleFunc("POST /api/sync", s.auth(s.handleSync))
	mux.HandleFunc("GET /api/events", s.auth(s.handleEvents))

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // embedded at build time; cannot fail at runtime
	}
	mux.Handle("GET /", http.FileServer(http.FS(sub)))

	return securityHeaders(mux)
}

// auth requires the startup token, accepted either as a query parameter — which
// is what an EventSource can send, since it cannot set headers — or as a bearer
// header for everything else.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.URL.Query().Get("token")
		if got == "" {
			if h := r.Header.Get("Authorization"); len(h) > 7 && h[:7] == "Bearer " {
				got = h[7:]
			}
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeError(w, http.StatusUnauthorized, "missing or invalid token")
			return
		}
		next(w, r)
	}
}

// securityHeaders keeps the dashboard from being embedded elsewhere or from
// leaking its token through a referrer.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		// No 'unsafe-inline' anywhere: the script and stylesheet are separate
		// files, and the few dynamic styles are set through the CSSOM, which CSP
		// does not restrict. font-src is needed because the typefaces are served
		// from this binary rather than a CDN.
		h.Set("Content-Security-Policy",
			"default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; "+
				"connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'")
		next.ServeHTTP(w, r)
	})
}

// Listen binds loopback only. Port 0 asks the kernel for a free port, which
// keeps the dashboard from colliding with anything else the user runs.
func (s *Server) Listen(port int) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
}

// URL is the address to hand the user, token included.
func (s *Server) URL(ln net.Listener) string {
	return fmt.Sprintf("http://%s/?token=%s", ln.Addr().String(), s.token)
}

// Serve runs until the context is cancelled.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go s.broker.run(ctx)

	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		// No write timeout: the SSE stream is deliberately long-lived.
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// OpenBrowser is best-effort: failing to launch a browser is not a reason to
// fail the command, since the URL was printed anyway.
func OpenBrowser(url string) {
	var cmd string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd = "explorer"
	default:
		cmd = "xdg-open"
	}
	_ = exec.Command(cmd, url).Start()
}

// ---------------------------------------------------------------- helpers

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The response is already committed; nothing useful is left to do.
		return
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
