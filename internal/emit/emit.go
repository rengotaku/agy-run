// Package emit writes the agy-run metadata contract: single-key
// {"agy_run":...} JSON Lines, all on the diagnostic (stderr) stream.
//
// Stream contract (Unix / clig.dev convention — consumed mechanically by
// callers: Claude / improve-loop / skills):
//   - stdout carries the agy body only — raw text, no markers, no JSON. The
//     body flows straight from agy's stdout, so `agy-run ... 2>/dev/null`
//     yields the body alone and EOF is the terminator (no begin/end markers).
//   - stderr carries every wrapper line: diagnostics and structured meta. Each
//     meta line is one JSON object whose sole top-level key is "agy_run"
//     (note/cooldown/error/timeout/stall/conversation/body_file/artifacts).
//
// All meta lines are produced with encoding/json so values are always correctly
// escaped, and — since they never touch stdout — can never collide with body
// text that happens to contain a `{"agy_run":...}`-shaped line.
package emit

import (
	"encoding/json"
	"fmt"
	"io"
)

// Emitter serializes metadata contract lines to the diagnostic (stderr) stream.
// The agy body itself is written directly to stdout by the runner (via an
// io.MultiWriter), never through the Emitter — keeping stdout body-only.
type Emitter struct {
	Err io.Writer // diagnostic/meta lines (all agy_run JSON + free-text log tails)
}

// New returns an Emitter writing to err (stderr).
func New(err io.Writer) *Emitter {
	return &Emitter{Err: err}
}

// line marshals fields into a single {"agy_run":<event>,...} JSON object and
// writes it (newline-terminated) to w. Marshalling never fails for the map
// shapes we build, but any error is surfaced to the caller for safety.
func line(w io.Writer, event string, fields map[string]any) error {
	obj := make(map[string]any, len(fields)+1)
	for k, v := range fields {
		obj[k] = v
	}
	obj["agy_run"] = event
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}

// Meta writes a contract line to the diagnostic (stderr) stream.
func (e *Emitter) Meta(event string, fields map[string]any) {
	_ = line(e.Err, event, fields)
}

// Plain writes free text (non-JSON) to the diagnostic stream. Used only for the
// timeout/stall log tail, which is intentionally not JSON-wrapped.
func (e *Emitter) Plain(s string) {
	_, _ = fmt.Fprint(e.Err, s)
}
