package index

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAppendWritesOneLinePerCall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "index.jsonl")
	Append(path, Record{Prompt: "one", ExitCode: 0, ConversationID: ConvPtr("c1")})
	Append(path, Record{Prompt: "two", ExitCode: 42})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := 0
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	if lines != 2 {
		t.Fatalf("lines = %d, want 2", lines)
	}

	// First record: schema defaulted, conversation_id present, artifacts [].
	var rec map[string]any
	first := data[:indexOfNL(data)]
	if err := json.Unmarshal(first, &rec); err != nil {
		t.Fatal(err)
	}
	if rec["schema"].(float64) != 1 {
		t.Errorf("schema = %v", rec["schema"])
	}
	if rec["conversation_id"] != "c1" {
		t.Errorf("conversation_id = %v", rec["conversation_id"])
	}
	if arts, ok := rec["artifacts"].([]any); !ok || len(arts) != 0 {
		t.Errorf("artifacts = %v, want []", rec["artifacts"])
	}
}

func TestAppendNullConversationWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.jsonl")
	Append(path, Record{Prompt: "x", ConversationID: ConvPtr("")})
	data, _ := os.ReadFile(path)
	var rec map[string]any
	if err := json.Unmarshal(data[:indexOfNL(data)], &rec); err != nil {
		t.Fatal(err)
	}
	if rec["conversation_id"] != nil {
		t.Errorf("conversation_id = %v, want null", rec["conversation_id"])
	}
}

func TestAppendPerms600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.jsonl")
	Append(path, Record{Prompt: "x"})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 600", perm)
	}
}

func TestAppendSandboxFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.jsonl")
	// Sandboxed run: sandbox=true and net_log present.
	Append(path, Record{Prompt: "sb", Sandbox: true, NetLog: "/x/net-access.jsonl"})
	// Un-sandboxed run: sandbox=false emitted, net_log omitted.
	Append(path, Record{Prompt: "plain"})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	nl := indexOfNL(data)
	var sb, plain map[string]any
	if err := json.Unmarshal(data[:nl], &sb); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data[nl+1:len(data)-1], &plain); err != nil {
		t.Fatal(err)
	}

	if sb["sandbox"] != true {
		t.Errorf("sandboxed run: sandbox = %v, want true", sb["sandbox"])
	}
	if sb["net_log"] != "/x/net-access.jsonl" {
		t.Errorf("sandboxed run: net_log = %v", sb["net_log"])
	}
	// false must still be emitted (no omitempty) so it is distinguishable from
	// a pre-#13 record that lacked the field entirely.
	if v, ok := plain["sandbox"]; !ok || v != false {
		t.Errorf("plain run: sandbox = %v (ok=%v), want false present", v, ok)
	}
	if _, ok := plain["net_log"]; ok {
		t.Errorf("plain run: net_log should be omitted, got %v", plain["net_log"])
	}
}

func indexOfNL(b []byte) int {
	for i, c := range b {
		if c == '\n' {
			return i
		}
	}
	return len(b)
}
