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

	"github.com/rengotaku/dotfiles-ubuntu/linux/cmd/agy-run/internal/config"
	"github.com/rengotaku/dotfiles-ubuntu/linux/cmd/agy-run/internal/emit"
	"github.com/rengotaku/dotfiles-ubuntu/linux/cmd/agy-run/internal/runner"
)

const usage = `agy-run: agy (Antigravity CLI) を安全に呼ぶラッパー (Go 版)。

使い方:
  agy-run [-t <dur>] [-m <model>] [-c | -C <conversation-id>] [-w <lock-wait-sec>]
          [-d] [-a <extra-arg>]... <prompt>
    -t  agy の --print-timeout（既定 240s。数値のみなら s を補完）
    -m  モデル名（例: "Gemini 3.5 Flash (High)"）
    -c  直近会話の継続
    -C  会話IDを指定して継続
    -w  ロック待ち秒数（既定 240）
    -d  --dangerously-skip-permissions を付与（読み取り/調査主体のタスク限定）
    -a  agy へそのまま渡す追加引数（繰り返し可。例: -a --add-dir -a /path/to/repo）

環境変数:
    AGY_RUN_MAX_CONCURRENCY  有界並列スロット数（既定 3）
    AGY_RUN_IDLE_LIMIT       アイドル上限秒（既定 600 = 10分。heartbeat 途絶で exit 44）
    AGY_RUN_ABS_LIMIT        絶対上限秒（既定 3600 = 60分。超過で exit 124）
    AGY_RUN_POLL             watchdog ポーリング秒（既定 20）

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
