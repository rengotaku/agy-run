package convlog

import (
	"fmt"
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

// A13: since が秒未満の端数を持つ場合でも、同じ秒の created_at を持つ呼び出しが正しくカウントされ artifact_missing が発火すること
func TestA13_SameSecondToolCallIsCounted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")

	// since has fractional seconds (.700Z)
	sinceTime := time.Date(2026, 8, 23, 1, 0, 6, 700000000, time.UTC)
	// created_at is exact same second (2026-08-23T01:00:06Z)
	createdAt := time.Date(2026, 8, 23, 1, 0, 6, 0, time.UTC).Format(time.RFC3339)

	entry := `{"created_at":"` + createdAt + `","type":"PLANNER_RESPONSE","tool_calls":[{"name":"generate_image"}]}` + "\n"

	if err := os.WriteFile(path, []byte(entry), 0o644); err != nil {
		t.Fatal(err)
	}

	count := CountArtifactToolCalls(path, sinceTime)
	if count != 1 {
		t.Errorf("count = %d, want 1 (same second tool call should be counted)", count)
	}
	err := CheckArtifactMissing(count, []string{})
	if err == nil || err.Reason != "artifact_missing" {
		t.Errorf("expected artifact_missing error, got %+v", err)
	}
}

// C1: -m 指定あり -> 従来どおり指定値が記録される（既存の記録形式を変えない）
func TestC1_ModelFlagSpecified(t *testing.T) {
	dir := t.TempDir()
	trPath := filepath.Join(dir, "transcript.jsonl")
	_ = os.WriteFile(trPath, []byte(`{"content":"The user changed setting `+"`"+`Model Selection`+"`"+` from None to Gemini 3.1 Pro (High)."}`), 0o644)
	setPath := filepath.Join(dir, "settings.json")
	_ = os.WriteFile(setPath, []byte(`{"model":"Gemini 3.1 Pro (High)"}`), 0o644)
	modelMap := map[string]string{"Gemini 3.1 Pro (High)": "gemini-3.1-pro-high"}

	got := ResolveEffectiveModel("gemini-3.6-flash-high", trPath, setPath, modelMap)
	if got != "gemini-3.6-flash-high" {
		t.Errorf("got %q, want %q", got, "gemini-3.6-flash-high")
	}
}

// C2: -m なし、transcript あり -> kebab-case ID が記録される（実効モデルを追えるか）
func TestC2_TranscriptModelSelectionNormalized(t *testing.T) {
	dir := t.TempDir()
	trPath := filepath.Join(dir, "transcript.jsonl")
	content := `{"content":"The user changed setting ` + "`" + `Model Selection` + "`" + ` from None to Gemini 3.6 Flash (High)."}` + "\n"
	_ = os.WriteFile(trPath, []byte(content), 0o644)
	setPath := filepath.Join(dir, "settings.json")
	modelMap := map[string]string{"Gemini 3.6 Flash (High)": "gemini-3.6-flash-high"}

	got := ResolveEffectiveModel("", trPath, setPath, modelMap)
	if got != "gemini-3.6-flash-high" {
		t.Errorf("got %q, want %q", got, "gemini-3.6-flash-high")
	}
}

// C3: -m なし、transcript あり (対応表未登録) -> 表示名がそのまま記録される（正規化に失敗しても空文字を残さない）
func TestC3_TranscriptModelSelectionUnregisteredDisplayName(t *testing.T) {
	dir := t.TempDir()
	trPath := filepath.Join(dir, "transcript.jsonl")
	content := `{"content":"The user changed setting ` + "`" + `Model Selection` + "`" + ` from None to Unknown Model (High)."}` + "\n"
	_ = os.WriteFile(trPath, []byte(content), 0o644)
	setPath := filepath.Join(dir, "settings.json")
	modelMap := map[string]string{"Gemini 3.6 Flash (High)": "gemini-3.6-flash-high"}

	got := ResolveEffectiveModel("", trPath, setPath, modelMap)
	if got != "Unknown Model (High)" {
		t.Errorf("got %q, want %q", got, "Unknown Model (High)")
	}
}

// C4: -m なし、transcript なし、settings.json の値あり -> settings.json の値（正規化）が記録される（代替経路が効くか）
func TestC4_SettingsJsonFallback(t *testing.T) {
	dir := t.TempDir()
	trPath := filepath.Join(dir, "non_existent.jsonl")
	setPath := filepath.Join(dir, "settings.json")
	_ = os.WriteFile(setPath, []byte(`{"model":"Gemini 3.1 Pro (High)"}`), 0o644)
	modelMap := map[string]string{"Gemini 3.1 Pro (High)": "gemini-3.1-pro-high"}

	got := ResolveEffectiveModel("", trPath, setPath, modelMap)
	if got != "gemini-3.1-pro-high" {
		t.Errorf("got %q, want %q", got, "gemini-3.1-pro-high")
	}
}

// C5: -m なし、transcript なし、settings.json なし -> "(unresolved)" が記録される（空文字を残さないか）
func TestC5_UnresolvedFallback(t *testing.T) {
	dir := t.TempDir()
	trPath := filepath.Join(dir, "non_existent.jsonl")
	setPath := filepath.Join(dir, "non_existent.json")

	got := ResolveEffectiveModel("", trPath, setPath, nil)
	if got != UnresolvedModel {
		t.Errorf("got %q, want %q", got, UnresolvedModel)
	}
}

// C6: 解決処理が失敗するケース -> 実行そのものは成功し既定の代替値 "(unresolved)" が記録される（記録のための処理が本来の実行を妨げないか）
func TestC6_ResolutionFailureSafety(t *testing.T) {
	dir := t.TempDir()
	// Pass a directory path as transcriptPath (os.Open will succeed or scan will fail/error)
	trPath := dir
	setPath := filepath.Join(dir, "invalid.json")
	_ = os.WriteFile(setPath, []byte(`{invalid json`), 0o644)

	got := ResolveEffectiveModel("", trPath, setPath, nil)
	if got != UnresolvedModel {
		t.Errorf("got %q, want %q", got, UnresolvedModel)
	}
}

// 追加テスト: C7_NormalizeModelName_NilMap / modelMap が nil の場合の安全動作確認
func TestC7_NormalizeModelName_NilMap(t *testing.T) {
	got := NormalizeModelName("Gemini 3.6 Flash (High)", nil)
	if got != "Gemini 3.6 Flash (High)" {
		t.Errorf("got %q, want %q", got, "Gemini 3.6 Flash (High)")
	}
}

// 追加テスト: C8_ExtractModelFromTranscript_MultipleChanges / 複数回の設定変更ログから最新モデルを取得することの確認
func TestC8_ExtractModelFromTranscript_MultipleChanges(t *testing.T) {
	dir := t.TempDir()
	trPath := filepath.Join(dir, "transcript.jsonl")
	line1 := `{"content":"The user changed setting ` + "`" + `Model Selection` + "`" + ` from None to Gemini 3.1 Pro (High)."}` + "\n"
	line2 := `{"content":"The user changed setting ` + "`" + `Model Selection` + "`" + ` from Gemini 3.1 Pro (High) to Gemini 3.6 Flash (High)."}` + "\n"
	_ = os.WriteFile(trPath, []byte(line1+line2), 0o644)

	got := ExtractModelFromTranscript(trPath)
	if got != "Gemini 3.6 Flash (High)" {
		t.Errorf("got %q, want %q", got, "Gemini 3.6 Flash (High)")
	}
}

// 追加テスト: C9_ExtractModelFromSettings_MalformedJson / 破損 JSON で空文字を返しパニックしないことの確認
func TestC9_ExtractModelFromSettings_MalformedJson(t *testing.T) {
	dir := t.TempDir()
	setPath := filepath.Join(dir, "settings.json")
	_ = os.WriteFile(setPath, []byte(`{invalid json`), 0o644)

	got := ExtractModelFromSettings(setPath)
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

// 追加テスト: C10_NormalizeModelName_AlreadyID / 既に ID 形式の名称が渡された場合にモデルマップから正しく返されるかの確認
func TestC10_NormalizeModelName_AlreadyID(t *testing.T) {
	modelMap := map[string]string{
		"Gemini 3.6 Flash (High)": "gemini-3.6-flash-high",
		"gemini-3.6-flash-high":   "gemini-3.6-flash-high",
	}
	got := NormalizeModelName("gemini-3.6-flash-high", modelMap)
	if got != "gemini-3.6-flash-high" {
		t.Errorf("got %q, want %q", got, "gemini-3.6-flash-high")
	}
}

// 回帰テスト: TestC11_RealTranscriptMarkerFormat / モデル名の後に文が続く実形式で誤った部分文字列を拾わないこと
func TestC11_RealTranscriptMarkerFormat(t *testing.T) {
	trPath := "testdata/transcript_real_marker.jsonl"
	modelMap := map[string]string{
		"Gemini 3.1 Pro (High)": "gemini-3.1-pro-high",
	}

	got := ResolveEffectiveModel("", trPath, "", modelMap)
	if got != "gemini-3.1-pro-high" {
		t.Errorf("got %q, want %q", got, "gemini-3.1-pro-high")
	}
}

// 回帰テスト: TestC13_QuotedSettingsMarkerIsIgnored / 会話中の引用でモデル記録が汚染されないこと
func TestC13_QuotedSettingsMarkerIsIgnored(t *testing.T) {
	trPath := "testdata/transcript_quoted_marker.jsonl"
	modelMap := map[string]string{
		"Gemini 3.1 Pro (High)": "gemini-3.1-pro-high",
		"Gemini 3.6 Flash (Low)": "gemini-3.6-flash-low",
	}

	got := ResolveEffectiveModel("", trPath, "", modelMap)
	if got != "gemini-3.1-pro-high" {
		t.Errorf("got %q, want %q", got, "gemini-3.1-pro-high")
	}
}

// D1: 同一ツールが閾値超の回数連続呼び出し（途中に schedule あり）、成果物増えていない -> warnings に記録される
func TestD1_StallDetected(t *testing.T) {
	dir := t.TempDir()
	tp := filepath.Join(dir, "transcript.jsonl")

	var sb strings.Builder
	// 17 calls of generate_image interleaved with 5 schedule calls
	// start: 2026-08-23T01:00:19Z, end: 2026-08-23T01:25:18Z (diff: 1499s)
	t0, _ := time.Parse(time.RFC3339, "2026-08-23T01:00:19Z")
	tEnd, _ := time.Parse(time.RFC3339, "2026-08-23T01:25:18Z")
	totalSteps := 22 // 17 generate_image + 5 schedule
	stepDuration := tEnd.Sub(t0) / time.Duration(totalSteps-1)

	stepIdx := 1
	for i := 0; i < 17; i++ {
		curTime := t0.Add(time.Duration(stepIdx-1) * stepDuration).Format(time.RFC3339)
		if i == 16 {
			curTime = tEnd.Format(time.RFC3339)
		}
		fmt.Fprintf(&sb, `{"type":"PLANNER_RESPONSE","status":"DONE","created_at":"%s","tool_calls":[{"name":"generate_image"}],"content":"","thinking":"","source":"MODEL","step_index":%d}`+"\n", curTime, stepIdx)
		stepIdx++
		if i < 5 {
			curTime = t0.Add(time.Duration(stepIdx-1) * stepDuration).Format(time.RFC3339)
			fmt.Fprintf(&sb, `{"type":"PLANNER_RESPONSE","status":"DONE","created_at":"%s","tool_calls":[{"name":"schedule"}],"content":"","thinking":"","source":"MODEL","step_index":%d}`+"\n", curTime, stepIdx)
			stepIdx++
		}
	}
	_ = os.WriteFile(tp, []byte(sb.String()), 0o600)

	warn := DetectToolStall(tp, []string{}, 10, time.Time{})
	if warn == "" {
		t.Fatalf("expected warning for stalled tool calls, got empty")
	}
	if !strings.Contains(warn, "generate_image") {
		t.Errorf("expected warning to mention 'generate_image', got %q", warn)
	}
	if !strings.Contains(warn, "17") {
		t.Errorf("expected warning to mention count '17', got %q", warn)
	}
	if !strings.Contains(warn, "1499") {
		t.Errorf("expected warning to mention elapsed seconds '1499', got %q", warn)
	}
}

// D2: 同一ツールが閾値未満の回数呼び出し -> 記録されない
func TestD2_BelowThreshold(t *testing.T) {
	dir := t.TempDir()
	tp := filepath.Join(dir, "transcript.jsonl")

	var sb strings.Builder
	t0, _ := time.Parse(time.RFC3339, "2026-08-23T01:00:19Z")
	for i := 0; i < 5; i++ {
		curTime := t0.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		fmt.Fprintf(&sb, `{"type":"PLANNER_RESPONSE","status":"DONE","created_at":"%s","tool_calls":[{"name":"generate_image"}],"content":"","thinking":"","source":"MODEL","step_index":%d}`+"\n", curTime, i+1)
	}
	_ = os.WriteFile(tp, []byte(sb.String()), 0o600)

	warn := DetectToolStall(tp, []string{}, 10, time.Time{})
	if warn != "" {
		t.Errorf("expected no warning for below threshold calls, got %q", warn)
	}
}

// D3: 同一ツールが閾値超だが途中で成果物が増えている -> 記録されない
func TestD3_ProgressWithArtifacts(t *testing.T) {
	dir := t.TempDir()
	tp := filepath.Join(dir, "transcript.jsonl")

	var sb strings.Builder
	t0, _ := time.Parse(time.RFC3339, "2026-08-23T01:00:19Z")
	for i := 0; i < 17; i++ {
		curTime := t0.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		fmt.Fprintf(&sb, `{"type":"PLANNER_RESPONSE","status":"DONE","created_at":"%s","tool_calls":[{"name":"generate_image"}],"content":"","thinking":"","source":"MODEL","step_index":%d}`+"\n", curTime, i+1)
	}
	_ = os.WriteFile(tp, []byte(sb.String()), 0o600)

	artifacts := []string{"/path/to/image.png"}
	warn := DetectToolStall(tp, artifacts, 10, time.Time{})
	if warn != "" {
		t.Errorf("expected no warning when artifacts exist, got %q", warn)
	}
}

// D4: 別々のツールが閾値超の回数呼び出し -> 記録されない
func TestD4_DifferentTools(t *testing.T) {
	dir := t.TempDir()
	tp := filepath.Join(dir, "transcript.jsonl")

	tools := []string{"find_by_name", "read_file", "write_to_file"}
	var sb strings.Builder
	t0, _ := time.Parse(time.RFC3339, "2026-08-23T01:00:19Z")
	for i := 0; i < 15; i++ {
		tool := tools[i%len(tools)]
		curTime := t0.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		fmt.Fprintf(&sb, `{"type":"PLANNER_RESPONSE","status":"DONE","created_at":"%s","tool_calls":[{"name":"%s"}],"content":"","thinking":"","source":"MODEL","step_index":%d}`+"\n", curTime, tool, i+1)
	}
	_ = os.WriteFile(tp, []byte(sb.String()), 0o600)

	warn := DetectToolStall(tp, []string{}, 10, time.Time{})
	if warn != "" {
		t.Errorf("expected no warning for alternating tools, got %q", warn)
	}
}

// D5: 閾値の環境変数を変える -> 判定が変わる
func TestD5_EnvironmentVariableThreshold(t *testing.T) {
	dir := t.TempDir()
	tp := filepath.Join(dir, "transcript.jsonl")

	var sb strings.Builder
	t0, _ := time.Parse(time.RFC3339, "2026-08-23T01:00:19Z")
	for i := 0; i < 7; i++ {
		curTime := t0.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		fmt.Fprintf(&sb, `{"type":"PLANNER_RESPONSE","status":"DONE","created_at":"%s","tool_calls":[{"name":"generate_image"}],"content":"","thinking":"","source":"MODEL","step_index":%d}`+"\n", curTime, i+1)
	}
	_ = os.WriteFile(tp, []byte(sb.String()), 0o600)

	// Threshold 10 (default): no warning
	warnDefault := DetectToolStall(tp, []string{}, 10, time.Time{})
	if warnDefault != "" {
		t.Errorf("expected no warning with threshold 10, got %q", warnDefault)
	}

	// Threshold 5 (custom): should warn
	warnCustom := DetectToolStall(tp, []string{}, 5, time.Time{})
	if warnCustom == "" {
		t.Errorf("expected warning with threshold 5, got empty")
	}
}

// D6: transcript が読めない -> 記録されずクラッシュしない
func TestD6_UnreadableTranscript(t *testing.T) {
	dir := t.TempDir()
	missingPath := filepath.Join(dir, "nonexistent.jsonl")
	warnMissing := DetectToolStall(missingPath, []string{}, 10, time.Time{})
	if warnMissing != "" {
		t.Errorf("expected no warning for nonexistent transcript, got %q", warnMissing)
	}

	brokenPath := filepath.Join(dir, "broken.jsonl")
	_ = os.WriteFile(brokenPath, []byte("invalid json {{{\n{also bad}\n"), 0o600)
	warnBroken := DetectToolStall(brokenPath, []string{}, 10, time.Time{})
	if warnBroken != "" {
		t.Errorf("expected no warning for broken transcript, got %q", warnBroken)
	}
}

// S1: run_command を 12 回連続 -> 成果物ツールではないツールでは警告しない
func TestS1_RunCommandNoWarning(t *testing.T) {
	dir := t.TempDir()
	tp := filepath.Join(dir, "transcript.jsonl")

	var sb strings.Builder
	t0, _ := time.Parse(time.RFC3339, "2026-08-23T01:00:19Z")
	for i := 0; i < 12; i++ {
		curTime := t0.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		fmt.Fprintf(&sb, `{"type":"PLANNER_RESPONSE","status":"DONE","created_at":"%s","tool_calls":[{"name":"run_command"}],"content":"","thinking":"","source":"MODEL","step_index":%d}`+"\n", curTime, i+1)
	}
	_ = os.WriteFile(tp, []byte(sb.String()), 0o600)

	warn := DetectToolStall(tp, []string{}, 10, time.Time{})
	if warn != "" {
		t.Errorf("expected no warning for run_command, got %q", warn)
	}
}

// S2: view_file を 12 回連続 -> 同上（別のツール名）
func TestS2_ViewFileNoWarning(t *testing.T) {
	dir := t.TempDir()
	tp := filepath.Join(dir, "transcript.jsonl")

	var sb strings.Builder
	t0, _ := time.Parse(time.RFC3339, "2026-08-23T01:00:19Z")
	for i := 0; i < 12; i++ {
		curTime := t0.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		fmt.Fprintf(&sb, `{"type":"PLANNER_RESPONSE","status":"DONE","created_at":"%s","tool_calls":[{"name":"view_file"}],"content":"","thinking":"","source":"MODEL","step_index":%d}`+"\n", curTime, i+1)
	}
	_ = os.WriteFile(tp, []byte(sb.String()), 0o600)

	warn := DetectToolStall(tp, []string{}, 10, time.Time{})
	if warn != "" {
		t.Errorf("expected no warning for view_file, got %q", warn)
	}
}

// S3: generate_image を 6 回、run_command を 1 回、generate_image を 6 回 -> 成果物ツールではないツールが連続を途切れさせる
func TestS3_NonArtifactToolBreaksStreak(t *testing.T) {
	dir := t.TempDir()
	tp := filepath.Join(dir, "transcript.jsonl")

	var sb strings.Builder
	t0, _ := time.Parse(time.RFC3339, "2026-08-23T01:00:19Z")
	tools := make([]string, 0, 13)
	for i := 0; i < 6; i++ {
		tools = append(tools, "generate_image")
	}
	tools = append(tools, "run_command")
	for i := 0; i < 6; i++ {
		tools = append(tools, "generate_image")
	}
	for i, tool := range tools {
		curTime := t0.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		fmt.Fprintf(&sb, `{"type":"PLANNER_RESPONSE","status":"DONE","created_at":"%s","tool_calls":[{"name":"%s"}],"content":"","thinking":"","source":"MODEL","step_index":%d}`+"\n", curTime, tool, i+1)
	}
	_ = os.WriteFile(tp, []byte(sb.String()), 0o600)

	warn := DetectToolStall(tp, []string{}, 10, time.Time{})
	if warn != "" {
		t.Errorf("expected no warning when streak is broken by run_command, got %q", warn)
	}
}

// S4: generate_image を 6 回、schedule を 2 回、generate_image を 6 回 -> schedule は連続を途切れさせない
func TestS4_ScheduleDoesNotBreakStreak(t *testing.T) {
	dir := t.TempDir()
	tp := filepath.Join(dir, "transcript.jsonl")

	var sb strings.Builder
	t0, _ := time.Parse(time.RFC3339, "2026-08-23T01:00:19Z")
	tools := make([]string, 0, 14)
	for i := 0; i < 6; i++ {
		tools = append(tools, "generate_image")
	}
	tools = append(tools, "schedule", "schedule")
	for i := 0; i < 6; i++ {
		tools = append(tools, "generate_image")
	}
	for i, tool := range tools {
		curTime := t0.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		fmt.Fprintf(&sb, `{"type":"PLANNER_RESPONSE","status":"DONE","created_at":"%s","tool_calls":[{"name":"%s"}],"content":"","thinking":"","source":"MODEL","step_index":%d}`+"\n", curTime, tool, i+1)
	}
	_ = os.WriteFile(tp, []byte(sb.String()), 0o600)

	warn := DetectToolStall(tp, []string{}, 10, time.Time{})
	if !strings.Contains(warn, "generate_image") || !strings.Contains(warn, "12 回") {
		t.Errorf("expected warning mentioning generate_image and 12 回, got %q", warn)
	}
}

// S5: run_command を 30 回、続けて generate_image を 10 回 -> 前に長い非成果物の連続があっても成果物ツールの連続だけを報告
func TestS5_LongNonArtifactPrefixThenArtifactStall(t *testing.T) {
	dir := t.TempDir()
	tp := filepath.Join(dir, "transcript.jsonl")

	var sb strings.Builder
	t0, _ := time.Parse(time.RFC3339, "2026-08-23T01:00:19Z")
	tools := make([]string, 0, 40)
	for i := 0; i < 30; i++ {
		tools = append(tools, "run_command")
	}
	for i := 0; i < 10; i++ {
		tools = append(tools, "generate_image")
	}
	for i, tool := range tools {
		curTime := t0.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		fmt.Fprintf(&sb, `{"type":"PLANNER_RESPONSE","status":"DONE","created_at":"%s","tool_calls":[{"name":"%s"}],"content":"","thinking":"","source":"MODEL","step_index":%d}`+"\n", curTime, tool, i+1)
	}
	_ = os.WriteFile(tp, []byte(sb.String()), 0o600)

	warn := DetectToolStall(tp, []string{}, 10, time.Time{})
	if !strings.Contains(warn, "generate_image") || !strings.Contains(warn, "10 回") || strings.Contains(warn, "run_command") {
		t.Errorf("expected warning mentioning generate_image and 10 回 without run_command, got %q", warn)
	}
}

// S6: generate_image を 12 回、続けて run_command を 1 回 -> 末尾の非成果物ツールで確定済みの連続が消えない
func TestS6_TrailingNonArtifactToolKeepsStall(t *testing.T) {
	dir := t.TempDir()
	tp := filepath.Join(dir, "transcript.jsonl")

	var sb strings.Builder
	t0, _ := time.Parse(time.RFC3339, "2026-08-23T01:00:19Z")
	tools := make([]string, 0, 13)
	for i := 0; i < 12; i++ {
		tools = append(tools, "generate_image")
	}
	tools = append(tools, "run_command")
	for i, tool := range tools {
		curTime := t0.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		fmt.Fprintf(&sb, `{"type":"PLANNER_RESPONSE","status":"DONE","created_at":"%s","tool_calls":[{"name":"%s"}],"content":"","thinking":"","source":"MODEL","step_index":%d}`+"\n", curTime, tool, i+1)
	}
	_ = os.WriteFile(tp, []byte(sb.String()), 0o600)

	warn := DetectToolStall(tp, []string{}, 10, time.Time{})
	if !strings.Contains(warn, "generate_image") || !strings.Contains(warn, "12 回") {
		t.Errorf("expected warning mentioning generate_image and 12 回, got %q", warn)
	}
}







