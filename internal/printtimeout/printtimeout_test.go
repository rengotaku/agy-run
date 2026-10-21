package printtimeout

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureLine = "W1016 16:44:11.396513       1 poll.go:188] Print mode: print timeout after 6m0s with turn in progress after 14216 polls (printed=803)"

// P1: 実ログの行を、前後に実ログ形式の別の行（I1016 ... printmode.go:202] Print mode: starting ... など）を置いた reader に入れる
// 手順: detect(r)
// 期待: ok == true、返る line が fixture 行と完全一致
// 検証観点: 正規表現が実ログ形式に通る
func TestP1(t *testing.T) {
	log := "I1016 16:38:11.123456       1 printmode.go:202] Print mode: starting ...\n" +
		fixtureLine + "\n" +
		"I1016 16:44:12.000000       1 main.go:100] exiting\n"

	line, ok := detect(strings.NewReader(log))
	if !ok {
		t.Fatalf("detect(r) ok = false, want true")
	}
	if line != fixtureLine {
		t.Fatalf("detect(r) line = %q, want %q", line, fixtureLine)
	}
}

// P2: user asked: what does "Print mode: print timeout after 8m0s with turn in progress" mean だけの reader
// 手順: detect(r)
// 期待: ok == false、line == ""
// 検証観点: 本文やプロンプト由来の同じ語句で誤検知しない（ソース位置タグが必須）
func TestP2(t *testing.T) {
	input := `user asked: what does "Print mode: print timeout after 8m0s with turn in progress" mean`
	line, ok := detect(strings.NewReader(input))
	if ok {
		t.Fatalf("detect(r) ok = true (line = %q), want false", line)
	}
	if line != "" {
		t.Fatalf("detect(r) line = %q, want empty string", line)
	}
}

// P3: 先頭が ERROR: logging before google.Init: のあとに、レベル文字を I / W / E にした同じ行
// 手順: detect(r) を 3 通り
// 期待: すべて ok == true
// 検証観点: glog のレベルや前置きに依存しない
func TestP3(t *testing.T) {
	levels := []string{"I", "W", "E"}
	for _, lvl := range levels {
		line := "ERROR: logging before google.Init: " + lvl + fixtureLine[1:]
		got, ok := detect(strings.NewReader(line))
		if !ok {
			t.Errorf("level %s: detect(r) ok = false, want true", lvl)
		}
		if got != line {
			t.Errorf("level %s: detect(r) line = %q, want %q", lvl, got, line)
		}
	}
}

// P4: 存在しないパス
// 手順: Detect(path)
// 期待: ok == false、panic しない
// 検証観点: 判定不能を失敗にしない
func TestP4(t *testing.T) {
	missingPath := filepath.Join(t.TempDir(), "nonexistent.log")
	line, ok := Detect(missingPath)
	if ok {
		t.Fatalf("Detect(%q) ok = true (line = %q), want false", missingPath, line)
	}
	if line != "" {
		t.Fatalf("Detect(%q) line = %q, want empty string", missingPath, line)
	}
}

// P5: 1 MiB + 1 バイトの 1 行（改行なし）のあとに fixture 行を置いたファイル
// 手順: Detect(path)
// 期待: ok == true、line が fixture 行
// 検証観点: 🔴 長い行で走査が止まらない（#52 の穴を引き継がない）
func TestP5(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "large.log")
	longLine := strings.Repeat("x", 1024*1024+1)
	content := longLine + "\n" + fixtureLine + "\n"
	if err := os.WriteFile(logPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok := Detect(logPath)
	if !ok {
		t.Fatalf("Detect(%q) ok = false, want true", logPath)
	}
	if got != fixtureLine {
		t.Fatalf("Detect(%q) line = %q, want %q", logPath, got, fixtureLine)
	}
}

// P6: 1 MiB + 1 バイトの 1 行だけのファイル
// 手順: Detect(path)
// 期待: ok == false、panic しない
// 検証観点: 長い行を読み捨てても落ちない
func TestP6(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "large_only.log")
	longLine := strings.Repeat("x", 1024*1024+1)
	if err := os.WriteFile(logPath, []byte(longLine), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok := Detect(logPath)
	if ok {
		t.Fatalf("Detect(%q) ok = true (line = %q), want false", logPath, got)
	}
	if got != "" {
		t.Fatalf("Detect(%q) line = %q, want empty string", logPath, got)
	}
}

// P7: fixture 行を 2 回（時間を 6m0s と 8m0s に変えて）含む reader
// 手順: detect(r)
// 期待: 最初の行が返る
// 検証観点: 結果が決定的
func TestP7(t *testing.T) {
	firstLine := fixtureLine
	secondLine := strings.Replace(fixtureLine, "6m0s", "8m0s", 1)
	input := firstLine + "\n" + secondLine + "\n"
	line, ok := detect(strings.NewReader(input))
	if !ok {
		t.Fatalf("detect(r) ok = false, want true")
	}
	if line != firstLine {
		t.Fatalf("detect(r) line = %q, want %q", line, firstLine)
	}
}

// P8: poll.go:188] Print mode: print timeout after 8m0s で止まり、with turn in progress が無い行
// 手順: detect(r)
// 期待: ok == false
// 検証観点: 未知の別形式を時間切れと断定しない
func TestP8(t *testing.T) {
	input := "W1016 16:44:11.396513       1 poll.go:188] Print mode: print timeout after 8m0s\n"
	line, ok := detect(strings.NewReader(input))
	if ok {
		t.Fatalf("detect(r) ok = true (line = %q), want false", line)
	}
	if line != "" {
		t.Fatalf("detect(r) line = %q, want empty string", line)
	}
}

// 追加テスト: TestDetect_FromFile / 検知するバグ: 実在する通常サイズのログファイルから Detect 経由で一致行を検出できないバグ
func TestDetect_FromFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agy.log")
	logContent := "I1016 16:38:11.123456 1 printmode.go:202] Print mode: starting ...\n" +
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

// 追加テスト: TestDetect_LongLineWithTrailingNewline / 検知するバグ: 改行ありの巨大行のみのログで終端判定やバッファ破棄に失敗して panic または誤判定するバグ
func TestDetect_LongLineWithTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "large_with_newline.log")
	longLine := strings.Repeat("x", 1024*1024+1) + "\n"
	if err := os.WriteFile(logPath, []byte(longLine), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok := Detect(logPath)
	if ok {
		t.Fatalf("Detect(%q) ok = true (line = %q), want false", logPath, got)
	}
	if got != "" {
		t.Fatalf("Detect(%q) line = %q, want empty string", logPath, got)
	}
}

// 追加テスト: TestDetect_BoundaryAndFalsePositives / 検知するバグ: 異なるソースファイル名やタグ不備のあるメッセージを誤検知するバグ
func TestDetect_BoundaryAndFalsePositives(t *testing.T) {
	cases := []string{
		// 異なるソースファイル名
		"W1016 16:44:11.396513 1 other.go:188] Print mode: print timeout after 6m0s with turn in progress",
		// タグなし
		"Print mode: print timeout after 6m0s with turn in progress",
	}
	for _, c := range cases {
		line, ok := detect(strings.NewReader(c))
		if ok {
			t.Errorf("detect(%q) ok = true (line = %q), want false", c, line)
		}
	}
}

type errReader struct {
	data string
	err  error
}

func (e *errReader) Read(p []byte) (n int, err error) {
	if len(e.data) > 0 {
		n = copy(p, e.data)
		e.data = e.data[n:]
		return n, nil
	}
	return 0, e.err
}

// 追加テスト: TestDetect_ReaderError / 検知するバグ: io.Reader から I/O エラーが発生した際（通常の読み取りおよび長大行の読み捨て中）に panic または誤った値を返すバグ
func TestDetect_ReaderError(t *testing.T) {
	// 1. 通常読み取り時のエラー
	r1 := &errReader{err: errors.New("read error")}
	line, ok := detect(r1)
	if ok || line != "" {
		t.Fatalf("detect(r1) = (%q, %v), want (\"\", false)", line, ok)
	}

	// 2. 長大行の読み捨てループ中のエラー
	r2 := &errReader{data: strings.Repeat("y", 10000), err: errors.New("stream closed during long line")}
	line, ok = detect(r2)
	if ok || line != "" {
		t.Fatalf("detect(r2) = (%q, %v), want (\"\", false)", line, ok)
	}
}
