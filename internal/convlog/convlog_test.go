package convlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveConvIDPrefersConversationEq(t *testing.T) {
	log := `Created conversation 11111111-1111-1111-1111-111111111111
some line conversation=22222222-2222-2222-2222-222222222222 more
conversation=33333333-3333-3333-3333-333333333333
`
	if got := resolveConvID(strings.NewReader(log)); got != "33333333-3333-3333-3333-333333333333" {
		t.Errorf("convID = %q", got)
	}
}

func TestResolveConvIDFallbackToCreated(t *testing.T) {
	log := "Created conversation abcdef01-2345-6789-abcd-ef0123456789\n"
	if got := resolveConvID(strings.NewReader(log)); got != "abcdef01-2345-6789-abcd-ef0123456789" {
		t.Errorf("convID = %q", got)
	}
}

func TestResolveConvIDNone(t *testing.T) {
	if got := resolveConvID(strings.NewReader("no ids here\n")); got != "" {
		t.Errorf("convID = %q, want empty", got)
	}
}

func TestResolveConvIDFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "log")
	_ = os.WriteFile(p, []byte("conversation=aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\n"), 0o600)
	if got := ResolveConvID(p); got != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Errorf("convID = %q", got)
	}
	if got := ResolveConvID(filepath.Join(dir, "missing")); got != "" {
		t.Errorf("missing file convID = %q", got)
	}
}

func TestTranscriptPath(t *testing.T) {
	got := TranscriptPath("/brain", "conv-1")
	want := filepath.Join("/brain", "conv-1", ".system_generated", "logs", "transcript.jsonl")
	if got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if TranscriptPath("/brain", "") != "" {
		t.Error("empty conv should yield empty path")
	}
}

func TestNewImageArtifacts(t *testing.T) {
	brain := t.TempDir()
	conv := "conv-1"
	dir := filepath.Join(brain, conv)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	old := filepath.Join(dir, "old.png")
	_ = os.WriteFile(old, []byte("x"), 0o600)
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(old, past, past)

	since := time.Now()
	time.Sleep(5 * time.Millisecond)

	newImg := filepath.Join(dir, "fresh.jpg")
	_ = os.WriteFile(newImg, []byte("y"), 0o600)
	notImg := filepath.Join(dir, "notes.txt")
	_ = os.WriteFile(notImg, []byte("z"), 0o600)

	got := NewImageArtifacts(brain, conv, since)
	if len(got) != 1 || got[0] != newImg {
		t.Errorf("artifacts = %v, want [%s]", got, newImg)
	}
}

func TestNewImageArtifactsEmptyConv(t *testing.T) {
	if got := NewImageArtifacts(t.TempDir(), "", time.Now()); got != nil {
		t.Errorf("empty conv = %v, want nil", got)
	}
}
