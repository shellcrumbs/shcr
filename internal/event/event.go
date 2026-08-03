// Package event defines the append-only event log types that are the source of
// truth for the entire system. Command rows are always derived from these.
package event

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type Type string

const (
	TypeStart Type = "start"
	TypeEnd   Type = "end"
	// TypeOrphan is emitted by the daemon's sweep when the shell that started a
	// command is gone but no end event ever arrived. It is an event rather than
	// a local column so the state survives the recompute and can sync to peers.
	TypeOrphan Type = "orphan"
	TypeRedact Type = "redact"
	// TypeImport is a command recovered from a shell's history file rather than
	// observed. It has no exit code and its time may be approximate.
	TypeImport Type = "import"
)

const RedactedMarker = "[REDACTED]"

// Event is the unit written to the log and, later, shipped to peers.
type Event struct {
	EventID   string          `json:"event_id"`
	CommandID string          `json:"command_id"`
	DeviceID  string          `json:"device_id"`
	Type      Type            `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt int64           `json:"created_at"` // unix millis
}

// StartPayload carries everything known at the moment a command begins.
type StartPayload struct {
	Command      string `json:"command"`
	Hostname     string `json:"hostname"`
	SessionID    string `json:"session_id"`
	Cwd          string `json:"cwd"`
	GitBranch    string `json:"git_branch,omitempty"`
	Shell        string `json:"shell"`
	StartTime    int64  `json:"start_time"`
	PGID         int    `json:"pgid"`
	IsBackground bool   `json:"is_background"`
}

// ImportPayload is what a history file can actually tell us. There is no exit
// code in any shell's history format, and only zsh records a start time by
// default — so the fields absent here are absent on purpose.
type ImportPayload struct {
	Command   string `json:"command"`
	Hostname  string `json:"hostname"`
	Shell     string `json:"shell"`
	StartTime int64  `json:"start_time"`
	// ApproximateTime marks a timestamp this tool derived from file position
	// rather than read from the history file.
	ApproximateTime bool `json:"approximate_time,omitempty"`
	// DurationMS is only ever set from zsh's elapsed field.
	DurationMS int64  `json:"duration_ms,omitempty"`
	Source     string `json:"source"`
}

// EndPayload carries the result of a completed command.
type EndPayload struct {
	EndTime  int64 `json:"end_time"`
	ExitCode int   `json:"exit_code"`
}

func NowMillis() int64 { return time.Now().UnixMilli() }

// NewID returns a UUIDv7, which sorts by creation time.
func NewID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}

func New(commandID, deviceID string, t Type, payload any) (Event, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}
	return Event{
		EventID:   NewID(),
		CommandID: commandID,
		DeviceID:  deviceID,
		Type:      t,
		Payload:   b,
		CreatedAt: NowMillis(),
	}, nil
}
