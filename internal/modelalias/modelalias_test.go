package modelalias

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Case M1: DefaultPath の優先順
// 前提: getenv はテスト内の map を引く関数。home は t.TempDir()
// 手順: (a) AGY_RUN_MODELS_FILE と XDG_CONFIG_HOME の両方あり (b) XDG_CONFIG_HOME だけあり (c) どちらも空
// 期待挙動: (a) AGY_RUN_MODELS_FILE の値 (b) <XDG_CONFIG_HOME>/agy-run/models.json (c) <home>/.config/agy-run/models.json
// 検証観点: 優先順の取り違え、空文字の環境変数を「設定あり」と扱うバグ
func TestM1_DefaultPath(t *testing.T) {
	home := t.TempDir()

	// (a) AGY_RUN_MODELS_FILE と XDG_CONFIG_HOME の両方あり
	envA := map[string]string{
		"AGY_RUN_MODELS_FILE": "/custom/path/models.json",
		"XDG_CONFIG_HOME":     "/xdg/home",
	}
	gotA := DefaultPath(func(k string) string { return envA[k] }, home)
	wantA := "/custom/path/models.json"
	if gotA != wantA {
		t.Errorf("(a) got %q, want %q", gotA, wantA)
	}

	// (b) XDG_CONFIG_HOME だけあり
	envB := map[string]string{
		"AGY_RUN_MODELS_FILE": "",
		"XDG_CONFIG_HOME":     "/xdg/home",
	}
	gotB := DefaultPath(func(k string) string { return envB[k] }, home)
	wantB := filepath.Join("/xdg/home", "agy-run", "models.json")
	if gotB != wantB {
		t.Errorf("(b) got %q, want %q", gotB, wantB)
	}

	// (c) どちらも空
	envC := map[string]string{
		"AGY_RUN_MODELS_FILE": "",
		"XDG_CONFIG_HOME":     "",
	}
	gotC := DefaultPath(func(k string) string { return envC[k] }, home)
	wantC := filepath.Join(home, ".config", "agy-run", "models.json")
	if gotC != wantC {
		t.Errorf("(c) got %q, want %q", gotC, wantC)
	}
}

// Case M2: Load の正常系
// 前提: {"chains":{"impl":["a","b"],"low":["c"]},"note":"x"} を TempDir に書く
// 手順: Load(path)
// 期待挙動: impl=[a,b]、low=[c]。note は無視されエラーにならない
// 検証観点: 未知のトップレベルキーで落ちないこと
func TestM2_Load(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	content := `{"chains":{"impl":["a","b"],"low":["c"]},"note":"x"}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	chains, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q) unexpected err: %v", path, err)
	}

	wantImpl := []string{"a", "b"}
	if !reflect.DeepEqual(chains["impl"], wantImpl) {
		t.Errorf("chains[%q] = %v, want %v", "impl", chains["impl"], wantImpl)
	}
	wantLow := []string{"c"}
	if !reflect.DeepEqual(chains["low"], wantLow) {
		t.Errorf("chains[%q] = %v, want %v", "low", chains["low"], wantLow)
	}
}

// Case M3: Load の異常系（テーブルテスト）
// 前提: ファイルが無い / { だけ / {"x":1}（chains 無し）/ {"chains":[]} / {"chains":{"impl":[]}} / {"chains":{"impl":[1]}} / {"chains":{"impl":[""]}}
// 手順: それぞれ Load(path)
// 期待挙動: すべて error。error の文字列にパスが入る
// 検証観点: 壊れた設定を空の連鎖として通し、agy の既定モデルで走ってしまう fail-open
func TestM3_Load_Errors(t *testing.T) {
	dir := t.TempDir()
	nonExistentPath := filepath.Join(dir, "non-existent.json")

	tests := []struct {
		name    string
		path    string
		content string
		create  bool
	}{
		{
			name:   "file does not exist",
			path:   nonExistentPath,
			create: false,
		},
		{
			name:    "broken json",
			path:    filepath.Join(dir, "broken.json"),
			content: "{",
			create:  true,
		},
		{
			name:    "missing chains",
			path:    filepath.Join(dir, "no_chains.json"),
			content: `{"x":1}`,
			create:  true,
		},
		{
			name:    "chains is array not object",
			path:    filepath.Join(dir, "chains_array.json"),
			content: `{"chains":[]}`,
			create:  true,
		},
		{
			name:    "empty chain array",
			path:    filepath.Join(dir, "empty_chain.json"),
			content: `{"chains":{"impl":[]}}`,
			create:  true,
		},
		{
			name:    "chain element not string",
			path:    filepath.Join(dir, "non_string_elem.json"),
			content: `{"chains":{"impl":[1]}}`,
			create:  true,
		},
		{
			name:    "chain element is empty string",
			path:    filepath.Join(dir, "empty_string_elem.json"),
			content: `{"chains":{"impl":[""]}}`,
			create:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.create {
				if err := os.WriteFile(tc.path, []byte(tc.content), 0644); err != nil {
					t.Fatalf("failed to write test file: %v", err)
				}
			}
			_, err := Load(tc.path)
			if err == nil {
				t.Fatalf("Load(%q) expected error, got nil", tc.path)
			}
			if !strings.Contains(err.Error(), tc.path) {
				t.Errorf("error %q does not contain path %q", err.Error(), tc.path)
			}
		})
	}
}

// Case M4: Resolve
// 前提: M2 と同じ Chains
// 手順: Resolve("impl") の戻り値の要素を書き換えてから、もう一度 Resolve("impl")。続けて Resolve("nope")
// 期待挙動: 2 回目も [a,b]（内部の slice を共有しない）。nope は *UnknownAliasError で、Known が ["impl","low"]（ソート済み）
// 検証観点: 呼び出し側の変更が設定を汚すバグ、hint に出す名前の順序の揺れ
func TestM4_Resolve(t *testing.T) {
	chains := Chains{
		"impl": []string{"a", "b"},
		"low":  []string{"c"},
	}

	res1, err := chains.Resolve("impl")
	if err != nil {
		t.Fatalf("Resolve(\"impl\") unexpected err: %v", err)
	}
	if !reflect.DeepEqual(res1, []string{"a", "b"}) {
		t.Fatalf("res1 = %v, want [a, b]", res1)
	}

	// 戻り値の要素を書き換える
	res1[0] = "mutated"

	// もう一度 Resolve("impl")
	res2, err := chains.Resolve("impl")
	if err != nil {
		t.Fatalf("Resolve(\"impl\") 2nd unexpected err: %v", err)
	}
	if !reflect.DeepEqual(res2, []string{"a", "b"}) {
		t.Errorf("res2 = %v, want [a, b] (internal slice was mutated)", res2)
	}

	// 続けて Resolve("nope")
	_, err = chains.Resolve("nope")
	if err == nil {
		t.Fatal("Resolve(\"nope\") expected error, got nil")
	}

	var unknownErr *UnknownAliasError
	if !errors.As(err, &unknownErr) {
		t.Fatalf("err is not *UnknownAliasError: %T (%v)", err, err)
	}
	if unknownErr.Name != "nope" {
		t.Errorf("unknownErr.Name = %q, want %q", unknownErr.Name, "nope")
	}
	wantKnown := []string{"impl", "low"}
	if !reflect.DeepEqual(unknownErr.Known, wantKnown) {
		t.Errorf("unknownErr.Known = %v, want %v", unknownErr.Known, wantKnown)
	}
}

// Case M5: FilterAvailable の順序保持
// 前提: chain=[x,a,y,b]、available=[a,b,c]
// 手順: FilterAvailable(chain, available)
// 期待挙動: kept=[a,b]、dropped=[x,y]（どちらも元の順）
// 検証観点: フォールバックの優先順が崩れるバグ
func TestM5_FilterAvailable(t *testing.T) {
	chain := []string{"x", "a", "y", "b"}
	available := []string{"a", "b", "c"}

	kept, dropped := FilterAvailable(chain, available)
	wantKept := []string{"a", "b"}
	wantDropped := []string{"x", "y"}

	if !reflect.DeepEqual(kept, wantKept) {
		t.Errorf("kept = %v, want %v", kept, wantKept)
	}
	if !reflect.DeepEqual(dropped, wantDropped) {
		t.Errorf("dropped = %v, want %v", dropped, wantDropped)
	}
}

// Case M6: ParseRef
// 前提: なし
// 手順: "@impl" / " @impl " / "gemini-3.5-flash-high" / "@impl,gemini-x" / "@" / "@Impl!" / "a,@impl" をそれぞれ渡す
// 期待挙動: ("impl", true, nil) / ("impl", true, nil) / ("", false, nil) / error / error / error / error
// 検証観点: 混在や不正な名前をモデル名として素通しするバグ
func TestM6_ParseRef(t *testing.T) {
	tests := []struct {
		input     string
		wantName  string
		wantIsRef bool
		wantErr   bool
	}{
		{input: "@impl", wantName: "impl", wantIsRef: true, wantErr: false},
		{input: " @impl ", wantName: "impl", wantIsRef: true, wantErr: false},
		{input: "gemini-3.5-flash-high", wantName: "", wantIsRef: false, wantErr: false},
		{input: "@impl,gemini-x", wantErr: true},
		{input: "@", wantErr: true},
		{input: "@Impl!", wantErr: true},
		{input: "a,@impl", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			name, isRef, err := ParseRef(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseRef(%q) expected error, got (%q, %v, nil)", tc.input, name, isRef)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRef(%q) unexpected error: %v", tc.input, err)
			}
			if name != tc.wantName || isRef != tc.wantIsRef {
				t.Errorf("ParseRef(%q) = (%q, %v), want (%q, %v)", tc.input, name, isRef, tc.wantName, tc.wantIsRef)
			}
		})
	}
}

// 追加テスト 1: 許可された文字（英小文字、数字、ハイフン、アンダースコア）が正しく受理されることの検証
func TestExtra_ParseRef_AllowedCharacters(t *testing.T) {
	tests := []struct {
		input    string
		wantName string
	}{
		{input: "@impl-fast", wantName: "impl-fast"},
		{input: "@impl_1", wantName: "impl_1"},
		{input: "@high-3_v2", wantName: "high-3_v2"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			name, isRef, err := ParseRef(tc.input)
			if err != nil {
				t.Fatalf("ParseRef(%q) unexpected err: %v", tc.input, err)
			}
			if !isRef || name != tc.wantName {
				t.Errorf("ParseRef(%q) = (%q, %v), want (%q, true)", tc.input, name, isRef, tc.wantName)
			}
		})
	}
}

// 追加テスト 2: FilterAvailable で全落ちおよび全残存の境界ケース
func TestExtra_FilterAvailable_EmptyOrDisjoint(t *testing.T) {
	// 全滅ケース
	kept1, dropped1 := FilterAvailable([]string{"x", "y"}, []string{"a", "b"})
	if len(kept1) != 0 {
		t.Errorf("kept1 = %v, want empty", kept1)
	}
	if !reflect.DeepEqual(dropped1, []string{"x", "y"}) {
		t.Errorf("dropped1 = %v, want [x y]", dropped1)
	}

	// 全残存ケース
	kept2, dropped2 := FilterAvailable([]string{"a", "b"}, []string{"a", "b", "c"})
	if !reflect.DeepEqual(kept2, []string{"a", "b"}) {
		t.Errorf("kept2 = %v, want [a b]", kept2)
	}
	if len(dropped2) != 0 {
		t.Errorf("dropped2 = %v, want empty", dropped2)
	}
}

// 追加テスト 3: 複数連鎖が存在するとき、後続連鎖の異常値も漏れなく検知できることの検証
func TestExtra_Load_MultipleChainsInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "multi_chains_invalid.json")
	content := `{"chains":{"valid":["a","b"],"invalid":[123]}}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatalf("Load(%q) expected error for invalid second chain, got nil", path)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not contain path %q", err.Error(), path)
	}
}

// 追加テスト 4: UnknownAliasError の Error() 文字列が alias 名と known リストを含むことの検証
func TestExtra_UnknownAliasError_Formatting(t *testing.T) {
	err := &UnknownAliasError{
		Name:  "nope",
		Known: []string{"impl", "low"},
	}
	msg := err.Error()
	if !strings.Contains(msg, "nope") || !strings.Contains(msg, "impl, low") {
		t.Errorf("unexpected error string: %q", msg)
	}
}

// 追加テスト 5: DefaultPath に nil の getenv が渡された場合も panic せず os.Getenv にフォールバックすることの検証
func TestExtra_DefaultPath_NilGetenv(t *testing.T) {
	home := t.TempDir()
	got := DefaultPath(nil, home)
	if got == "" {
		t.Error("DefaultPath(nil, home) returned empty string")
	}
}

// 追加テスト 6: {"chains":null} の場合にエラーとなりパスが含まれることの検証
func TestExtra_Load_ChainsNull(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chains_null.json")
	content := `{"chains":null}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatalf("Load(%q) expected error for null chains, got nil", path)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not contain path %q", err.Error(), path)
	}
}
