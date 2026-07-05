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

使い方:
  agy-run [-t <dur>] [-m <model[,fallback...]>] [-c | -C <conversation-id>]
          [-w <lock-wait-sec>] [-W <workspace-dir>] [--wait-quota]
          [-d] [-a <extra-arg>]... <prompt>
    -t  agy の --print-timeout（既定 240s。数値のみなら s を補完）
    -m  モデル名（例: "Gemini 3.5 Flash (High)"。agy models と完全一致必須。
        不一致は実行前に exit 2 で弾き、error.available_models に候補を返す）
        --model も可。カンマ区切りでフォールバック候補を並べると、
        rate_limited 時に順に再試行し、使用モデルを result.model に返す。
    -c  直近会話の継続
    -C  会話IDを指定して継続
    -w  ロック待ち秒数（既定 240）
    -W  agy の作業ディレクトリ（cmd.Dir）を固定（--workspace も可）。
        agy の RUN_COMMAND(build/test) の cwd 誤解決を防ぐ。対象リポの main
        worktree を渡すのが推奨。省略時は呼び出し元 cwd を継承。
    -d  --dangerously-skip-permissions を付与（読み取り/調査主体のタスク限定）
    -a  agy へそのまま渡す追加引数（繰り返し可。例: -a --add-dir -a /path/to/repo）
    --wait-quota  rate_limited かつログに "Resets in ..." があれば、リセットまで
        1回だけ待機して再試行（上限 AGY_RUN_MAX_QUOTA_WAIT）。

注意（agy の書き込み先）:
    agy は起動 cwd を無視し trustedWorkspaces のルート（container/main）に書く。
    feature worktree から呼ぶと差分は main 側に落ちるため、agy-run は起動前に
    食い違いを検出して result.warnings に警告を出す。実装後の手移送が必要。

環境変数:
    AGY_RUN_MAX_CONCURRENCY  有界並列スロット数（既定 3）
    AGY_RUN_IDLE_LIMIT       アイドル上限秒（既定 600 = 10分。heartbeat 途絶で exit 44）
    AGY_RUN_ABS_LIMIT        絶対上限秒（既定 3600 = 60分。超過で exit 124）
    AGY_RUN_POLL             watchdog ポーリング秒（既定 20）
    AGY_RUN_MAX_QUOTA_WAIT   --wait-quota の待機上限秒（既定 14400 = 4時間）

出力:
    1回の実行 = stdout に JSON 1個。stderr は cooldown 通知や
    stall/timeout のログ末尾など人向けテキストのみ。
    {
      "body":                 "agy の回答本文",
      "conversation_id":      "継続用 ID（agy-run -C <id>）",
      "body_file":            "本文の決定論的コピー先パス",
      "artifacts":            ["生成画像の絶対パス", ...],
      "model":                "実際に使用したモデル（-m 指定時）",
      "warnings":             ["非致命の警告（workspace 不一致 / RUN_COMMAND 失敗）", ...],
      "run_command_failures": 0,   // agy の build/test 失敗件数（>0 は自己申告要再検証）
      "exit_code":            0,
      "error":                null  // 失敗時のみ {"reason": ...}
    }
    例: agy-run "質問" 2>/dev/null | jq -r .body        # 本文だけ
        agy-run "質問" 2>/dev/null | jq -r .conversation_id
        agy-run "質問" 2>/dev/null | jq -r '.warnings[]?'

exit code: 0=成功 / 2=引数エラー / 4=ロック待ちタイムアウト /
           42=rate_limited / 44=アイドルストール / 124=絶対上限 / その他=agy 自身
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
