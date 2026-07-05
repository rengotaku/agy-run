package emit

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestMetaStreamIsJSON(t *testing.T) {
	var errb bytes.Buffer
	e := New(&errb)
	e.Meta("conversation", map[string]any{"id": "abc", "pid": 123})

	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(errb.Bytes()), &m); err != nil {
		t.Fatalf("meta not json: %v (%q)", err, errb.String())
	}
	if m["agy_run"] != "conversation" {
		t.Errorf("agy_run = %v", m["agy_run"])
	}
	if m["pid"].(float64) != 123 {
		t.Errorf("pid = %v", m["pid"])
	}
}

func TestSingleKeyOrderingAlwaysHasAgyRun(t *testing.T) {
	var errb bytes.Buffer
	e := New(&errb)
	// A field that would collide with agy_run must not override the event.
	e.Meta("error", map[string]any{"agy_run": "SHOULD_NOT_WIN", "reason": "x"})
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(errb.Bytes()), &m); err != nil {
		t.Fatal(err)
	}
	if m["agy_run"] != "error" {
		t.Errorf("agy_run = %v, want error", m["agy_run"])
	}
}

func TestPlainGoesToErr(t *testing.T) {
	var errb bytes.Buffer
	e := New(&errb)
	e.Plain("tail line\n")
	if errb.String() != "tail line\n" {
		t.Errorf("plain = %q", errb.String())
	}
}
