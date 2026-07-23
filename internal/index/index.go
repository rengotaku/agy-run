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
	"strconv"
	"strings"
	"syscall"

	"github.com/rengotaku/agy-run/internal/config"
)

// envIndexMaxBytes is the rotation-threshold knob (bytes; 0 disables
// rotation). Read directly from the environment here rather than threaded
// through Config, so Append's signature (and its two runner.go call sites)
// stay unchanged (issue #22).
const envIndexMaxBytes = "AGY_RUN_INDEX_MAX_BYTES"

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
	// Sandbox records whether this run executed under bwrap (--sandbox /
	// AGY_RUN_SANDBOX=1). Emitted unconditionally (no omitempty) so a false
	// value explicitly marks an un-sandboxed run rather than being
	// indistinguishable from a pre-#13 record that lacked the field.
	Sandbox bool `json:"sandbox"`
	// NetLog is the absolute path of the net-access.jsonl this run appended to;
	// present only in sandbox mode (empty/omitted otherwise).
	NetLog string `json:"net_log,omitempty"`
	// ErrorClass mirrors emit.Error.Reason for a failed run (rate_limited,
	// stall, timeout, lock_timeout, unknown_model, ...), letting index.jsonl be
	// diagnosed after the fact instead of every failure looking like an
	// indistinguishable blank exit_code (issue #22). Empty/omitted on a
	// successful run.
	ErrorClass string `json:"error_class,omitempty"`
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
	rotateIfNeeded(path, indexMaxBytes())
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

// indexMaxBytes resolves the rotation threshold from AGY_RUN_INDEX_MAX_BYTES,
// falling back to config.DefaultIndexMaxBytes when unset or unparsable. "0"
// (explicitly) disables rotation.
func indexMaxBytes() int64 {
	v := strings.TrimSpace(os.Getenv(envIndexMaxBytes))
	if v == "" {
		return config.DefaultIndexMaxBytes
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return config.DefaultIndexMaxBytes
	}
	return n
}

// rotateIfNeeded shifts index.jsonl -> .1 -> .2 -> .3 (dropping the oldest
// generation) once the current file has reached maxBytes, keeping the file
// bounded instead of growing forever (issue #22). maxBytes<=0 disables
// rotation entirely.
//
// Best-effort like the rest of this package: any failure (missing file,
// read-only parent dir, ...) is swallowed. If the final rename (current ->
// .1) fails, the current file is left in place so Append can still open it
// for writing — rotation must never block the side-channel record itself.
//
// Concurrency: several agy-run processes can reach this at the same moment;
// without serialization their .3<-.2<-.1<-current rename shifts interleave
// and can silently drop a whole generation. The critical section is guarded
// by a non-blocking flock rather than a blocking one — Append is a
// best-effort side channel and must never stall a real run waiting on
// another process's rotation, so a losing process simply skips rotation this
// round and retries on its next Append call.
func rotateIfNeeded(path string, maxBytes int64) {
	if maxBytes <= 0 {
		return
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() < maxBytes {
		return
	}
	unlock, ok := tryRotationLock(path)
	if !ok {
		return
	}
	defer unlock()

	// Re-stat after acquiring the lock: another process may have already
	// rotated (or appended) while we waited for the fd, so the file we saw
	// above may no longer qualify.
	info, err = os.Stat(path)
	if err != nil || info.Size() < maxBytes {
		return
	}

	_ = os.Remove(path + ".3")
	_ = os.Rename(path+".2", path+".3")
	_ = os.Rename(path+".1", path+".2")
	if err := os.Rename(path, path+".1"); err != nil {
		return
	}
	_ = os.Chmod(path+".1", 0o600)
}

// tryRotationLock attempts a non-blocking exclusive flock on
// "<path>.rotate.lock" (created alongside index.jsonl at 0600). ok=false
// means another process currently holds it (or the lock file could not be
// opened); the caller must skip rotation for this Append rather than wait.
func tryRotationLock(path string) (unlock func(), ok bool) {
	lockPath := path + ".rotate.lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, false
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, true
}
