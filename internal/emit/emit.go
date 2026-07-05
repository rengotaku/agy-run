// Package emit builds the agy-run output contract: exactly one JSON Result
// object on stdout describing the whole outcome of an invocation (body plus all
// metadata a caller needs), and free-text human diagnostics on stderr.
//
// Contract (consumed mechanically by callers — Claude / improve-loop / skills):
//   - stdout: one indented JSON object. Read the answer with `jq -r .body`, the
//     continuation id with `.conversation_id`, image paths with `.artifacts[]`,
//     and branch on `.exit_code` / `.error`.
//   - stderr: optional human advisories (cooldown notice, stall/timeout log
//     tail). Never required for machine consumption.
//
// One object per run replaces the old scheme of several {"agy_run":...} JSON
// lines interleaved with raw body — no stream-ordering races, no begin/end
// markers, no body/meta collision.
package emit

import (
	"encoding/json"
	"fmt"
	"io"
)

// Result is the single JSON object agy-run writes to stdout: the complete
// outcome of one invocation. Zero-valued optional fields are omitted.
type Result struct {
	Body           string   `json:"body"`
	ConversationID string   `json:"conversation_id,omitempty"`
	ContinueCmd    string   `json:"continue_cmd,omitempty"`
	BodyFile       string   `json:"body_file,omitempty"`
	Artifacts      []string `json:"artifacts,omitempty"`
	ExitCode       int      `json:"exit_code"`
	Error          *Error   `json:"error,omitempty"`
}

// Error is the structured failure detail embedded in Result.Error (nil on
// success). Reason is always set; the remaining fields are populated per
// failure kind and omitted when zero.
type Error struct {
	Reason           string `json:"reason"`
	Detail           string `json:"detail,omitempty"`
	Hint             string `json:"hint,omitempty"`
	Note             string `json:"note,omitempty"`
	LogRatelimitHits int    `json:"log_ratelimit_hits,omitempty"`
	IdleLimitSec     int    `json:"idle_limit_sec,omitempty"`
	AbsLimitSec      int    `json:"abs_limit_sec,omitempty"`
	PrintTimeout     string `json:"print_timeout,omitempty"`
	WaitSec          int    `json:"wait_sec,omitempty"`
	MaxConcurrency   int    `json:"max_concurrency,omitempty"`
}

// Emitter writes the Result to out (stdout) and human diagnostics to err (stderr).
type Emitter struct {
	Out io.Writer
	Err io.Writer
}

// New returns an Emitter writing to out (stdout) and err (stderr).
func New(out, err io.Writer) *Emitter {
	return &Emitter{Out: out, Err: err}
}

// Emit writes r as a single indented JSON object (newline-terminated) to stdout.
// Marshalling never fails for these shapes, but on the impossible error we still
// emit a well-formed object rather than a partial write.
func (e *Emitter) Emit(r Result) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		b = []byte(fmt.Sprintf(`{"body":"","exit_code":%d,"error":{"reason":"marshal_failed"}}`, r.ExitCode))
	}
	_, _ = fmt.Fprintf(e.Out, "%s\n", b)
}

// Note writes a free-text human advisory line (newline-terminated) to stderr.
func (e *Emitter) Note(s string) {
	_, _ = fmt.Fprintln(e.Err, s)
}

// Plain writes free text (verbatim, no added newline) to stderr — used for the
// stall/timeout log tail.
func (e *Emitter) Plain(s string) {
	_, _ = fmt.Fprint(e.Err, s)
}
