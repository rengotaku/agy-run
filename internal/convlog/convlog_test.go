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

// A1: transcript に generate_image 呼び出しがあり、成果物 0 件の場合 -> error.reason == "artifact_missing"
func TestA1_ArtifactMissing_GenerateImageWithNoArtifacts(t *testing.T) {
	err := CheckArtifactMissing(1, []string{})
	if err == nil {
		t.Fatalf("expected error for 1 tool call with 0 artifacts, got nil")
	}
	if err.Reason != "artifact_missing" {
		t.Errorf("reason = %q, want %q", err.Reason, "artifact_missing")
	}
}

// A2: generate_image 呼び出しがあり成果物 1 件以上の場合 -> error は nil
func TestA2_GenerateImageWithArtifacts(t *testing.T) {
	err := CheckArtifactMissing(1, []string{"/brain/conv-1/image.png"})
	if err != nil {
		t.Errorf("expected nil error when artifacts exist, got %+v", err)
	}
}

// A3: 成果物系ツールを一切呼ばないテキスト応答 -> 判定は発火しない (error は nil)
func TestA3_NoArtifactToolCalls(t *testing.T) {
	err := CheckArtifactMissing(0, []string{})
	if err != nil {
		t.Errorf("expected nil error when 0 tool calls, got %+v", err)
	}
}

// A4: transcript ファイルが存在しない / 読めない -> CountArtifactToolCalls は 0 を返し判定は発火しない
func TestA4_CountArtifactToolCalls_MissingFile(t *testing.T) {
	count := CountArtifactToolCalls("testdata/non_existent.jsonl", time.Time{})
	if count != 0 {
		t.Errorf("count = %d, want 0 for non-existent file", count)
	}
	err := CheckArtifactMissing(count, nil)
	if err != nil {
		t.Errorf("expected nil error for 0 count, got %+v", err)
	}
}

// A5: transcript に不正 JSON / 途切れがある -> CountArtifactToolCalls はクラッシュせず復旧/復元する
func TestA5_CountArtifactToolCalls_MalformedJSON(t *testing.T) {
	// Should not panic on malformed JSONL fixture
	count := CountArtifactToolCalls("testdata/transcript_malformed.jsonl", time.Time{})
	// Count can be equal to valid lines parsed or 0, but must not panic.
	_ = count
}

// A6: conversation ID が取得できない (transcriptPath == "") -> 判定は発火せず
func TestA6_CountArtifactToolCalls_EmptyConvID(t *testing.T) {
	path := TranscriptPath("/brain", "")
	if path != "" {
		t.Fatalf("expected empty path for empty convID, got %q", path)
	}
	count := CountArtifactToolCalls(path, time.Time{})
	if count != 0 {
		t.Errorf("count = %d, want 0 for empty path", count)
	}
	err := CheckArtifactMissing(count, nil)
	if err != nil {
		t.Errorf("expected nil error, got %+v", err)
	}
}

// 追加テスト: MultipleToolCallsWithArtifacts / 成果物系ツールが複数回呼ばれ成果物がある場合
func TestA8_MultipleToolCallsWithArtifacts(t *testing.T) {
	err := CheckArtifactMissing(3, []string{"/brain/conv-1/image1.png", "/brain/conv-1/image2.png"})
	if err != nil {
		t.Errorf("expected nil error for multiple tool calls with artifacts, got %+v", err)
	}
}

// 追加テスト: TranscriptWithOtherToolCallsOnly / 成果物以外のツールのみを含む場合は0を返す
func TestA9_TranscriptWithOtherToolCallsOnly(t *testing.T) {
	count := CountArtifactToolCalls("testdata/transcript_other_tools.jsonl", time.Time{})
	if count != 0 {
		t.Errorf("count = %d, want 0 for transcript with non-artifact tools only", count)
	}
}

// 追加テスト: TranscriptToolCallsCaseSensitivity / ツール名が完全一致しない場合（例: 大文字）はカウントされない
func TestA10_TranscriptToolCallsCaseSensitivity(t *testing.T) {
	// Checks that tool name matching is exact string match
	if ArtifactTools["Generate_Image"] {
		t.Errorf("ArtifactTools should not match upper-case tool names")
	}
}

// 追加テスト: CountArtifactToolCalls_ValidFile / 正常な generate_image 呼び出しを含む fixture で正しくカウントする
func TestA11_CountArtifactToolCalls_ValidFile(t *testing.T) {
	count := CountArtifactToolCalls("testdata/transcript_with_image.jsonl", time.Time{})
	if count != 1 {
		t.Errorf("count = %d, want 1 for transcript_with_image.jsonl", count)
	}
}

// A12: -C 継続でテキスト応答だけしたときに、since より前の generate_image 呼び出しを無視して正常判定されること
func TestA12_ContinuedConversationIgnoresPastToolCalls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")

	pastTime := time.Date(2026, 8, 23, 1, 0, 0, 0, time.UTC)
	sinceTime := time.Date(2026, 8, 23, 2, 0, 0, 0, time.UTC)
	currentTime := time.Date(2026, 8, 23, 2, 5, 0, 0, time.UTC)

	// past entry with generate_image
	entry1 := `{"created_at":"` + pastTime.Format(time.RFC3339) + `","type":"PLANNER_RESPONSE","tool_calls":[{"name":"generate_image"}]}` + "\n"
	// current entry after since with text response only
	entry2 := `{"created_at":"` + currentTime.Format(time.RFC3339) + `","type":"PLANNER_RESPONSE","content":"Here is text response"}` + "\n"

	if err := os.WriteFile(path, []byte(entry1+entry2), 0o644); err != nil {
		t.Fatal(err)
	}

	count := CountArtifactToolCalls(path, sinceTime)
	if count != 0 {
		t.Errorf("count = %d, want 0 (calls before since should be ignored)", count)
	}
	err := CheckArtifactMissing(count, []string{})
	if err != nil {
		t.Errorf("expected nil error, got %+v", err)
	}
}


