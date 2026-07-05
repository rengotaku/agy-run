package emit

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestBodyAndMetaStreams(t *testing.T) {
	var out, errb bytes.Buffer
	e := New(&out, &errb)
	e.Body("begin", map[string]any{"pid": 123})
	e.Meta("note", map[string]any{"event": "concurrent_agy_process", "safe": true})

	var begin map[string]any
	if err := json.Unmarshal(out.Bytes(), &begin); err != nil {
		t.Fatalf("out not json: %v (%q)", err, out.String())
	}
	if begin["agy_run"] != "begin" {
		t.Errorf("agy_run = %v", begin["agy_run"])
	}
	if begin["pid"].(float64) != 123 {
		t.Errorf("pid = %v", begin["pid"])
	}
	if !strings.Contains(errb.String(), `"agy_run":"note"`) {
		t.Errorf("meta missing: %q", errb.String())
	}
}

func TestSingleKeyOrderingAlwaysHasAgyRun(t *testing.T) {
	var out bytes.Buffer
	e := New(&out, &out)
	// A field that would collide with agy_run must not override the event.
	e.Body("end", map[string]any{"agy_run": "SHOULD_NOT_WIN", "exit_code": 0})
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["agy_run"] != "end" {
		t.Errorf("agy_run = %v, want end", m["agy_run"])
	}
}

func TestBlankAndPlain(t *testing.T) {
	var out, errb bytes.Buffer
	e := New(&out, &errb)
	e.Blank()
	e.Plain("tail line\n")
	if out.String() != "\n" {
		t.Errorf("blank = %q", out.String())
	}
	if errb.String() != "tail line\n" {
		t.Errorf("plain = %q", errb.String())
	}
}
