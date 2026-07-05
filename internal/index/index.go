// Package index appends one record per agy-run invocation to index.jsonl
// (the pointer that links session <-> conversation <-> prompt <-> duration,
// read by agy-review). One call = one line. File perms are forced to 600.
//
// Index writes are best-effort: a failure here must never change agy-run's own
// exit code, so Append swallows its errors (returns nil on any I/O problem).
package index

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Record is one index.jsonl line. Field names/shape match the bash jq record
// (schema:1). ConversationID is emitted as null when empty.
type Record struct {
	Schema          int      `json:"schema"`
	TSStart         string   `json:"ts_start"`
	TSEnd           string   `json:"ts_end"`
	DurationMS      int64    `json:"duration_ms"`
	ClaudeSessionID string   `json:"claude_session_id"`
	ChildSessionID  string   `json:"child_session_id"`
	ConversationID  *string  `json:"conversation_id"`
	Continued       bool     `json:"continued"`
	Model           string   `json:"model"`
	CWD             string   `json:"cwd"`
	Prompt          string   `json:"prompt"`
	ExitCode        int      `json:"exit_code"`
	Artifacts       []string `json:"artifacts"`
}

// Append marshals rec and appends it as one line to path. The parent dir is
// created if needed; the file is created/kept at mode 0600. All errors are
// intentionally suppressed (best-effort side channel).
func Append(path string, rec Record) {
	if rec.Schema == 0 {
		rec.Schema = 1
	}
	if rec.Artifacts == nil {
		rec.Artifacts = []string{}
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	// Enforce 600 even if the file pre-existed with looser perms.
	_ = f.Chmod(0o600)
	b = append(b, '\n')
	_, _ = f.Write(b)
}

// ConvPtr returns a *string for the conversation id, or nil when empty (so the
// JSON encodes null, matching the bash record).
func ConvPtr(convID string) *string {
	if convID == "" {
		return nil
	}
	return &convID
}
