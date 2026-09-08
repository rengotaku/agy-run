package authreq

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureLine = "ERROR: logging before google.Init: I0907 21:31:30.280284       1 printmode.go:387] Print mode: triggering interactive OAuth"

// Case 1: authreq.detect は実ログ形式の一致行を検出する
// 前提: 上の fixture 行を含む数行（前後に printmode.go:364] Print mode: not authenticated, trying silent auth などの実ログ行を置く）の reader
// 手順: detect(r) を呼ぶ
// 期待挙動: ok == true、返る line が fixture 行と完全一致
// 検証観点: 正規表現が実ログ形式（glog prefix + ソース位置タグ）に通ること
func TestCase1_DetectMatchesRealLog(t *testing.T) {
	log := "ERROR: logging before google.Init: I0907 21:31:29.123456       1 printmode.go:364] Print mode: not authenticated, trying silent auth\n" +
		"ERROR: logging before google.Init: I0907 21:31:29.654321       1 printmode.go:370] Print mode: silent auth failed\n" +
		fixtureLine + "\n" +
		"I0907 21:31:31.000000       1 main.go:100] exiting\n"

	line, ok := detect(strings.NewReader(log))
	if !ok {
		t.Fatalf("detect(r) ok = false, want true")
	}
	if line != fixtureLine {
		t.Fatalf("detect(r) line = %q, want %q", line, fixtureLine)
	}
}

// Case 2: ソース位置タグの無い同名文字列は拾わない
// 前提: Print mode: triggering interactive OAuth を含むが printmode.go: タグの無い行（例: user asked: what does "Print mode: triggering interactive OAuth" mean）だけの reader
// 手順: detect(r)
// 期待挙動: ok == false
// 検証観点: 調査タスクの本文・プロンプト由来の文字列で誤検知しないこと（ADR 0001 の懸念）
func TestCase2_NoSourceTagIgnored(t *testing.T) {
	input := `user asked: what does "Print mode: triggering interactive OAuth" mean`
	line, ok := detect(strings.NewReader(input))
	if ok {
		t.Fatalf("detect(r) ok = true (line = %q), want false", line)
	}
	if line != "" {
		t.Fatalf("detect(r) line = %q, want empty string", line)
	}
}

// Case 3: ログが無い / 読めないときは検知しない
// 前提: 存在しない path
// 手順: Detect(path)
// 期待挙動: ok == false、panic しない
// 検証観点: 判定不能を失敗にしない（ADR 0001「壊すと危ない前提」）
func TestCase3_MissingOrUnreadableLog(t *testing.T) {
	missingPath := filepath.Join(t.TempDir(), "nonexistent.log")
	line, ok := Detect(missingPath)
	if ok {
		t.Fatalf("Detect(%q) ok = true (line = %q), want false", missingPath, line)
	}
	if line != "" {
		t.Fatalf("Detect(%q) line = %q, want empty string", missingPath, line)
	}
}

// 追加テスト: TestDetect_FromFile / authreq.Detect で実在するログファイルからの一致行検出と行内容の照合を検証
func TestDetect_FromFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agy.log")
	logContent := "I0907 21:31:29.123456 1 main.go:10] start\n" +
		fixtureLine + "\n"
	if err := os.WriteFile(logPath, []byte(logContent), 0o600); err != nil {
		t.Fatal(err)
	}
	line, ok := Detect(logPath)
	if !ok {
		t.Fatalf("Detect(%q) ok = false, want true", logPath)
	}
	if line != fixtureLine {
		t.Fatalf("Detect(%q) line = %q, want %q", logPath, line, fixtureLine)
	}
}

// 追加テスト: TestDetect_BoundaryAndFalsePositives / ソースファイル名違いや行末に余分な文字があるケースの誤検知排除を検証
func TestDetect_BoundaryAndFalsePositives(t *testing.T) {
	cases := []string{
		// 異なるファイル名
		"ERROR: I0907 21:31:30.280284 1 other.go:387] Print mode: triggering interactive OAuth",
		// 行末に余計な文字列
		"ERROR: I0907 21:31:30.280284 1 printmode.go:387] Print mode: triggering interactive OAuth extra",
		// 行末にクオートや記号
		`printmode.go:387] Print mode: triggering interactive OAuth"`,
	}
	for _, c := range cases {
		line, ok := detect(strings.NewReader(c))
		if ok {
			t.Errorf("detect(%q) ok = true (line = %q), want false", c, line)
		}
	}
}
