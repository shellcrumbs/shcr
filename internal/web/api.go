package web

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/shellcrumbs/shcr/internal/config"
	"github.com/shellcrumbs/shcr/internal/event"
	"github.com/shellcrumbs/shcr/internal/store"
)

const (
	defaultLimit = 100
	maxLimit     = 1000
	sessionPeers = 5
)

func (s *Server) handleCommands(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.Filter{
		Text:      q.Get("q"),
		Hostname:  q.Get("host"),
		Status:    q.Get("status"),
		SessionID: q.Get("session"),
		Cwd:       q.Get("cwd"),
		Limit:     clampInt(q.Get("limit"), defaultLimit, 1, maxLimit),
		Offset:    clampInt(q.Get("offset"), 0, 0, 1<<30),
	}
	if v := q.Get("since"); v != "" {
		f.Since, _ = strconv.ParseInt(v, 10, 64)
	}
	// `before` paginates by time, which is stabler than an offset when new
	// commands are landing while the user reads.
	if v := q.Get("before"); v != "" {
		f.Until, _ = strconv.ParseInt(v, 10, 64)
	}

	cmds, err := s.Store.QueryCommands(f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if cmds == nil {
		cmds = []store.Command{}
	}
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	runs, err := s.Store.RunsToday(midnight.UnixMilli())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"commands": cmds,
		"limit":    f.Limit,
		"runs":     runs,
	})
}

func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, err := s.Store.CommandByID(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if c == nil {
		writeError(w, http.StatusNotFound, "no such command")
		return
	}
	// What else was happening in that shell is usually the reason you opened
	// the command in the first place.
	before, err := s.Store.SessionContext(c.SessionID, c.StartTime, sessionPeers)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if before == nil {
		before = []store.Command{}
	}
	after, err := s.Store.SessionAfter(c.SessionID, c.StartTime, sessionPeers)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if after == nil {
		after = []store.Command{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"command":        c,
		"session_before": before,
		"session_after":  after,
		// Output capture is not implemented, and saying so plainly is better
		// than an empty panel the user reads as a bug.
		"output_captured": false,
	})
}

func (s *Server) handleRedact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, err := s.Store.CommandByID(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if c == nil {
		writeError(w, http.StatusNotFound, "no such command")
		return
	}
	// An event, not an in-place edit, so the redaction travels to every other
	// machine on the next sync.
	ev, err := event.New(c.ID, s.DeviceID, event.TypeRedact, map[string]any{
		"reason":      "dashboard",
		"redacted_at": event.NowMillis(),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := s.Store.AppendEvent(ev); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	updated, err := s.Store.CommandByID(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"command": updated})
}

func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.Store.Hostnames()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if hosts == nil {
		hosts = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": hosts})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.Store.Stats(time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// Device is one machine in the sync set.
type Device struct {
	DeviceID     string `json:"device_id"`
	Hostname     string `json:"hostname,omitempty"`
	IsThisDevice bool   `json:"is_this_device"`
	LastSyncedAt int64  `json:"last_synced_at,omitempty"`
	Commands     int64  `json:"commands"`
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	cursors, err := s.Store.Cursors()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	counts, err := s.Store.CommandsPerDevice()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	devices := []Device{{
		DeviceID:     s.DeviceID,
		Hostname:     s.Hostname,
		IsThisDevice: true,
		Commands:     counts[s.DeviceID],
	}}
	for _, c := range cursors {
		devices = append(devices, Device{
			DeviceID:     c.PeerDeviceID,
			Hostname:     c.HostnameHint,
			LastSyncedAt: c.LastSyncedAt,
			Commands:     counts[c.PeerDeviceID],
		})
	}
	// The page shortens paths under this machine's home to ~, the same as the
	// terminal surfaces do. Only the local home is sent: a peer's layout is not
	// ours to guess at.
	home, _ := os.UserHomeDir()
	writeJSON(w, http.StatusOK, map[string]any{"devices": devices, "home": home})
}

// Settings is the subset of configuration the dashboard may see. The encryption
// key and the recovery phrase are deliberately absent: the browser never needs
// them, so they never cross the wire.
type Settings struct {
	SyncEnabled   bool   `json:"sync_enabled"`
	SyncBackend   string `json:"sync_backend,omitempty"`
	SyncPath      string `json:"sync_path,omitempty"`
	ShareHostname bool   `json:"share_hostname"`
	SyncAvailable bool   `json:"sync_available"`
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, Settings{
		SyncEnabled:   cfg.Sync.Enabled,
		SyncBackend:   cfg.Sync.Backend,
		SyncPath:      cfg.Sync.Path,
		ShareHostname: cfg.Sync.ShareHostname,
		// Read from the configuration as it stands, not from whether a sync
		// function happened to be wired up when the server started.
		SyncAvailable: s.Sync != nil && cfg.Sync.Backend != "",
	})
}

func (s *Server) handlePatchSettings(w http.ResponseWriter, r *http.Request) {
	// Only the fields present in the body are touched, so a dashboard that
	// knows about fewer settings than the config file cannot erase the rest.
	var patch struct {
		SyncEnabled   *bool `json:"sync_enabled"`
		ShareHostname *bool `json:"share_hostname"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "malformed body: "+err.Error())
		return
	}
	cfg, err := config.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if patch.SyncEnabled != nil {
		if *patch.SyncEnabled && cfg.Sync.Backend == "" {
			writeError(w, http.StatusBadRequest,
				"sync has no backend configured yet; run `shcr sync enable --dir <path>` first")
			return
		}
		cfg.Sync.Enabled = *patch.SyncEnabled
	}
	if patch.ShareHostname != nil {
		cfg.Sync.ShareHostname = *patch.ShareHostname
	}
	if err := config.Save(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.handleGetSettings(w, r)
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	if s.Sync == nil {
		writeError(w, http.StatusPreconditionFailed, "sync is not configured on this machine")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	pushed, pulled, err := s.Sync(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pushed": pushed, "pulled": pulled})
}

func clampInt(raw string, def, lo, hi int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return min(max(n, lo), hi)
}
