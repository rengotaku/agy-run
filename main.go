// Command agy-run is a safe wrapper around the agy (Antigravity CLI) binary.
//
// The output contract is a single JSON object on stdout describing the whole
// outcome (body plus conversation/body_file/artifacts/exit_code/error); human
// diagnostics go to stderr. Process supervision — bounded-parallel slot
// locking, post-kill cooldown, process-group kill, and the idle+absolute
// watchdog — lives in Go, where signals, concurrency, and structured JSON are
// the right tools for the job.
//
// Exit codes: 0 success / 2 usage / 4 slot lock timeout / 42 rate_limited /
// 44 idle stall / 124 absolute timeout / else agy's own code.
package main

import (
	"fmt"
	"os"

	"github.com/rengotaku/agy-run/internal/config"
	"github.com/rengotaku/agy-run/internal/emit"
	"github.com/rengotaku/agy-run/internal/runner"
)

const usage = `agy-run: agy (Antigravity CLI) を安全に呼ぶラッパー (Go 版)。

生の "agy -p" はハング（承認待ち・print-timeout 無視・kill 直後の exit 144）や
出力の不安定さがある。agy-run はプロセス監視と 1実行=stdout に JSON 1個 の出力契約で
これを潰す。利用の詳細（フラグ・自動制御・出力・exit code）はこのヘルプが正典。

使い方:
  agy-run [-t <dur>] [-m <model[,fallback...]>] [-c | -C <conversation-id>]
          [-w <lock-wait-sec>] [-W <workspace-dir>] [--wait-quota]
          [-d] [-a <extra-arg>]... [--sandbox] <prompt>

フラグ:
    -t  agy の --print-timeout（既定 240s。数値のみなら s を補完）。
        ※ agy 自身へ渡すだけ。外側の総枠は下記 idle/abs watchdog が別に効く
    -m  モデル名。"agy models" の文字列と完全一致必須（バージョン・大文字小文字・括弧まで）。
        記憶で書かず迷ったら "agy models" を実行して正確な名前を取る。
        不一致は実行前に exit 2 で弾き、error.available_models[] に候補を返す
        （サイレントに別モデルで走ることはない）。未指定なら agy 既定モデル（--model も可）。
        カンマ区切りでフォールバック候補を並べると、rate_limited 時に順に再試行し、
        実際に使ったモデルを result.model に返す。
        例: "Gemini 3.5 Flash (High)" / "Gemini 3.1 Pro (High)"
    -c  直近会話の継続
    -C  会話ID (<conversation-id>) を指定して継続（前回結果 JSON の conversation_id）
    -w  スロットロック待ち秒数（既定 240）。空きスロットを取れなければ exit 4
    -W  agy の作業ディレクトリ（cmd.Dir）を固定（--workspace も可）。
        agy の RUN_COMMAND(build/test) の cwd 誤解決を防ぐ。対象リポの main
        worktree を渡すのが推奨。省略時は呼び出し元 cwd を継承。
    -d  --dangerously-skip-permissions を付与（読み取り/調査主体のタスク限定）
    -a  agy へそのまま渡す追加引数（繰り返し可。例: -a --add-dir -a /path/to/repo）
    --wait-quota  rate_limited かつログに "Resets in ..." があれば、リセットまで
        1回だけ待機して再試行（上限 AGY_RUN_MAX_QUOTA_WAIT）。
    --sandbox  bwrap でファイルシステム隔離した中で agy を実行し、全 HTTP(S) 通信を
        ~/.gemini/antigravity-cli/log/net-access.jsonl に記録する（通信は遮断しない）。
        AGY_RUN_SANDBOX=1 でも有効化可。bwrap 未導入なら実行前に exit 2（詳細は README）。
    -h  このヘルプを表示

注意（agy の書き込み先）:
    agy は起動 cwd を無視し trustedWorkspaces のルート（container/main）に書く。
    feature worktree から呼ぶと差分は main 側に落ちるため、agy-run は起動前に
    食い違いを検出して result.warnings に警告を出す。実装後の手移送が必要。

ラッパーが自動でやること:
    - アイドル watchdog（本質的な防御）: heartbeat（log / body / transcript.jsonl の
      最新 mtime）が AGY_RUN_IDLE_LIMIT 途絶えたら stall と判定し、プロセスグループごと
      kill → exit 44。総経過が AGY_RUN_ABS_LIMIT を超えたら kill → exit 124。
      agy 自身の --print-timeout は無視されることがある（実証: 90s 指定で6分+ 0出力）ため
      この外側監視が要。ポーリングは AGY_RUN_POLL 秒ごと。
    - クールダウン: kill 後 30 秒は次回実行を自動待機（kill 直後再実行の exit 144 即死を防止）
    - 有界並列: agy-run 同士を N スロット（AGY_RUN_MAX_CONCURRENCY・既定 3）で並列化。
      超過分は空きスロット待ち → 取れなければ exit 4
    - 会話継続: 結果 JSON に conversation_id を出す → -C <id> で文脈を保った追撃ができる
    - 生成画像の検出: agy は本文にパスを出さない（「生成しました」等のテキストのみ）ため、
      今回の実行で新規作成された画像を検出し artifacts[]（絶対パス配列）で返す
    - モデルフォールバック / quota 待機: -m "A,B" で rate_limited 時に次候補へ自動再試行。
      --wait-quota でログの "Resets in ..." を読み、リセットまで 1 回だけ待機して再試行
    - RUN_COMMAND 監査: agy の build/test 失敗をログから拾い run_command_failures /
      warnings に出す（agy の自己申告「テスト緑」の誤信を出力側から防ぐ）
    - 単一 JSON 出力: 実行結果を stdout に 1 個の JSON にまとめる（encoding/json で常にエスケープ）

環境変数:
    AGY_RUN_MAX_CONCURRENCY  有界並列スロット数（既定 3）
    AGY_RUN_IDLE_LIMIT       アイドル上限秒（既定 600 = 10分。heartbeat 途絶で exit 44）
    AGY_RUN_ABS_LIMIT        絶対上限秒（既定 3600 = 60分。超過で exit 124）
    AGY_RUN_POLL             watchdog ポーリング秒（既定 20）
    AGY_RUN_MAX_QUOTA_WAIT   --wait-quota の待機上限秒（既定 14400 = 4時間）
    AGY_RUN_SANDBOX          --sandbox と同じ（1/true/yes などで有効）
    AGY_RUN_SANDBOX_RO_BINDS 追加の読み取り専用 bind（コロン区切り、存在しないパスは無視）
    AGY_RUN_SANDBOX_RW_BINDS 追加の書き込み可 bind（コロン区切り、存在しないパスは無視）

出力（stdout = JSON 1個。stderr は cooldown 通知や stall/timeout のログ末尾など人向けのみ）:
    {
      "body":                 "agy の回答本文",
      "conversation_id":      "継続用 ID（agy-run -C <id>）",
      "body_file":            "本文の決定論的コピー先パス（純本文を読むならこのファイル）",
      "artifacts":            ["生成画像の絶対パス", ...],
      "model":                "実際に使用したモデル（-m 指定時）",
      "warnings":             ["非致命の警告（workspace 不一致 / RUN_COMMAND 失敗）", ...],
      "run_command_failures": 0,    // agy の build/test 失敗件数（>0 は自己申告要再検証）
      "net_log":              "/path/to/net-access.jsonl",  // --sandbox 時のみ
      "exit_code":            0,
      "error":                null   // 失敗時のみ {"reason": ..., "detail": ...}
    }
    例: agy-run "質問" 2>/dev/null | jq -r .body             # 本文だけ取り出す
        agy-run "質問" 2>/dev/null | jq -r .conversation_id  # 継続用 ID を取る
        agy-run -C <id> "続きの質問"                          # 同じ会話に追撃
        agy-run -m "Gemini 3.5 Flash (High)" -t 120s "軽い質問"
        agy-run "質問" 2>/dev/null | jq -r '.artifacts[]'    # 生成画像のパス列挙
        agy-run "質問" 2>/dev/null | jq -r '.warnings[]?'    # 警告を列挙

exit code:
    0=成功 / 2=引数エラー / 4=スロットロック待ちタイムアウト /
    42=レート制限（本文ゼロ＋ログに 429）/ 44=アイドルストール（heartbeat 途絶）/
    124=絶対上限タイムアウト / その他=agy 自身の exit code
`

func main() {
	e := emit.New(os.Stdout, os.Stderr)

	cfg, wantHelp, err := config.Parse(os.Args[1:], os.Getenv)
	if err != nil {
		e.Emit(emit.Result{ExitCode: config.ExitUsage, Error: &emit.Error{
			Reason: "bad_option", Detail: err.Error(),
		}})
		fmt.Fprint(os.Stderr, usage)
		os.Exit(config.ExitUsage)
	}
	if wantHelp {
		fmt.Print(usage)
		os.Exit(config.ExitOK)
	}
	if cfg.Prompt == "" {
		e.Emit(emit.Result{ExitCode: config.ExitUsage, Error: &emit.Error{Reason: "missing_prompt"}})
		fmt.Fprint(os.Stderr, usage)
		os.Exit(config.ExitUsage)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		e.Emit(emit.Result{ExitCode: config.ExitUsage, Error: &emit.Error{
			Reason: "no_home", Detail: err.Error(),
		}})
		os.Exit(config.ExitUsage)
	}

	rc := runner.Run(cfg, runner.Options{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Home:   home,
		Getenv: os.Getenv,
	})
	os.Exit(rc)
}
