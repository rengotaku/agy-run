package emit

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestEmitSingleObjectToStdout(t *testing.T) {
	var out, errb bytes.Buffer
	e := New(&out, &errb)
	e.Emit(Result{
		Body:           "the answer\nsecond line\n",
		ConversationID: "abc-123",
		BodyFile:       "/tmp/last-body/abc-123.md",
		Artifacts:      []string{"/tmp/x.jpg"},
		ExitCode:       0,
	})

	var r Result
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("stdout not a single JSON object: %v (%q)", err, out.String())
	}
	if r.Body != "the answer\nsecond line\n" {
		t.Errorf("body = %q", r.Body)
	}
	if r.ConversationID != "abc-123" || r.BodyFile == "" || len(r.Artifacts) != 1 {
		t.Errorf("meta lost: %+v", r)
	}
	if r.Error != nil {
		t.Errorf("error should be nil, got %+v", r.Error)
	}
	if errb.Len() != 0 {
		t.Errorf("stderr should be empty on success, got %q", errb.String())
	}
}

func TestEmitErrorObject(t *testing.T) {
	var out, errb bytes.Buffer
	e := New(&out, &errb)
	e.Emit(Result{ExitCode: 42, Error: &Error{Reason: "rate_limited", LogRatelimitHits: 3}})

	var r Result
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.ExitCode != 42 || r.Error == nil || r.Error.Reason != "rate_limited" {
		t.Errorf("error result = %+v", r)
	}
}

func TestNoteAndPlainGoToStderr(t *testing.T) {
	var out, errb bytes.Buffer
	e := New(&out, &errb)
	e.Note("cooldown: waiting 5s")
	e.Plain("log tail\n")
	if out.Len() != 0 {
		t.Errorf("stdout should be empty, got %q", out.String())
	}
	if errb.String() != "cooldown: waiting 5s\nlog tail\n" {
		t.Errorf("stderr = %q", errb.String())
	}
}
