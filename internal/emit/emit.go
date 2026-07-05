// Package emit writes the agy-run output contract: single-key {"agy_run":...}
// JSON Lines for wrapper metadata, and raw body text between begin/end markers.
//
// Contract (consumed mechanically by callers — Claude / improve-loop / skills):
//   - every wrapper line is one JSON object whose sole top-level key is "agy_run"
//   - the agy body is the raw text between the "begin" and "end" lines
//   - meta lines (note/cooldown/error/timeout/stall/conversation/body_file/
//     artifacts) never appear between begin and end
//
// All lines are produced with encoding/json so values are always correctly
// escaped (this replaces the hand-built echo/jq lines of the bash version and
// removes the class of jq-escaping bugs).
package emit

import (
	"encoding/json"
	"fmt"
	"io"
)

// Emitter serializes contract lines to an output (stdout) and error (stderr)
// stream. Body text flows to the Out stream between Begin and End.
type Emitter struct {
	Out io.Writer // contract lines that are part of the body stream (begin/end/conversation/body_file/artifacts)
	Err io.Writer // diagnostic/meta lines (note/cooldown/error/timeout/stall)
}

// New returns an Emitter writing to out (stdout) and err (stderr).
func New(out, err io.Writer) *Emitter {
	return &Emitter{Out: out, Err: err}
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

// Meta writes a contract line to the error (diagnostic) stream.
func (e *Emitter) Meta(event string, fields map[string]any) {
	_ = line(e.Err, event, fields)
}

// Body writes a contract line to the output (body) stream.
func (e *Emitter) Body(event string, fields map[string]any) {
	_ = line(e.Out, event, fields)
}

// Blank writes a bare newline to the output stream (matches the bash spacing
// before the conversation footer).
func (e *Emitter) Blank() {
	_, _ = fmt.Fprintln(e.Out)
}

// Plain writes free text (non-JSON) to the error stream. Used only for the
// timeout/stall log tail, which is intentionally not JSON-wrapped.
func (e *Emitter) Plain(s string) {
	_, _ = fmt.Fprint(e.Err, s)
}
