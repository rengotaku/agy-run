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
// 43 artifact_missing / 44 idle stall / 45 auth_required / 124 absolute timeout / else agy's own code.
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
          [-d] [-a <extra-arg>]... [--no-sandbox] <prompt>

フラグ:
    -t  agy の --print-timeout（既定 240s。数値のみなら s を補完）。
        ※ agy 自身へ渡すだけ。外側の総枠は下記 idle/abs watchdog が別に効く
    -m  モデル名または "@<名前>" によるエイリアス連鎖参照（--model も可）。
        ・モデル名直書き:
          "agy models" の文字列と完全一致必須（kebab-case ID、"-" の位置まで）。
          不一致は実行前に exit 2 (reason: unknown_model) で弾き、error.available_models[] に候補を返す。
          カンマ区切りでフォールバック候補を並べると、rate_limited 時に順に再試行し、
          実際に使ったモデルを result.model に返す。未指定なら agy 既定モデル。
          例: "gemini-3.8-flash-high" / "gemini-3.1-pro-high"
        ・エイリアス連鎖参照 (-m @<名前>):
          名前は [a-z0-9_-]+。models.json から候補リスト（chains）を引く。
          models.json の探索順:
            1. $AGY_RUN_MODELS_FILE
            2. $XDG_CONFIG_HOME/agy-run/models.json
            3. ~/.config/agy-run/models.json
          形式: {"chains": {"<名前>": ["<モデル ID>", ...]}}
          @ 参照のときだけ、"agy models" に無い候補を外して残りで実行し、
          外した名前を結果 JSON の dropped_models に記録する（直書きでは外さず exit 2）。
          @ 参照時の exit 2 の reason:
            - invalid_model_alias: @ のみ、モデル名直書きとの混在、使用不能文字
            - models_file_error:   models.json 不在、読取失敗、壊れた JSON、chains 不正
            - unknown_model_alias: 指定した名前が chains に未定義（hint に定義済みエイリアスをソートして表示）
            - no_available_model:  連鎖内の全候補が agy models に無く全滅
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
    --sandbox / --no-sandbox
        sandbox（bwrap でファイルシステム隔離した中で agy を実行し、全 HTTP(S) 通信を
        ~/.gemini/antigravity-cli/log/net-access.jsonl に記録。通信は遮断しない）は既定 ON。
        --no-sandbox または AGY_RUN_SANDBOX=0 で無効化（優先順位: フラグ > env > 既定ON）。
        bwrap 未導入の環境では警告を1件 result.warnings に載せた上で非サンドボックスへ
        自動的に降格して実行する（exit 2 では失敗させない。詳細は README）。
        host の GH_TOKEN（無ければ GITHUB_TOKEN）はサンドボックス内へ再注入され、
        gh/git の GitHub 認証に使える（~/.config/gh は bind しない）。
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
    - レート制限検出: 本文ゼロ時は exit 42 (rate_limited) とするが、本文非空時は exit code / error
      を変えず warnings にヒット件数を記録する（調査タスク等での誤検知による中断を防ぐ）
    - 認証要求の即時検知: 非対話 print mode 中に agy が interactive OAuth に入った場合、
      ハングを待たずに即時 kill → exit 45 (auth_required)
    - 単一 JSON 出力: 実行結果を stdout に 1 個の JSON にまとめる（encoding/json で常にエスケープ）

環境変数:
    AGY_RUN_MAX_CONCURRENCY  有界並列スロット数（既定 3）
    AGY_RUN_IDLE_LIMIT       アイドル上限秒（既定 600 = 10分。heartbeat 途絶で exit 44）
    AGY_RUN_ABS_LIMIT        絶対上限秒（既定 3600 = 60分。超過で exit 124）
    AGY_RUN_POLL             watchdog ポーリング秒（既定 20）
    AGY_RUN_MAX_QUOTA_WAIT   --wait-quota の待機上限秒（既定 14400 = 4時間）
    AGY_RUN_TOOL_REPEAT_LIMIT 同一ツール連続呼び出しの空転検出閾値（既定 10）
    AGY_RUN_SANDBOX          sandbox の on/off（0/false で無効化、1/true/yes などで有効化）。
                             フラグ未指定時のみ参照（既定は ON）
    AGY_RUN_SANDBOX_RO_BINDS 追加の読み取り専用 bind（コロン区切り、存在しないパスは無視）
    AGY_RUN_SANDBOX_RW_BINDS 追加の書き込み可 bind（コロン区切り、存在しないパスは無視）
    AGY_RUN_SANDBOX_NO_DBUS  1/true/yes/on で D-Bus セッションソケットを sandbox に bind
                             しない。issue #19 の systemd-run --user 経由の escape 経路
                             （-d 実行時にモデルがサンドボックス外へコマンドを飛ばせる）
                             を塞ぐ。既定は bind する（agy の OAuth トークンが Secret
                             Service にあると "Authentication required" で落ちるため。
                             issue #11）。Secret Service に依存しない環境で有効化推奨

出力（stdout = JSON 1個。stderr は cooldown 通知や stall/timeout のログ末尾など人向けのみ）:
    {
      "body":                 "agy の回答本文",
      "conversation_id":      "継続用 ID（agy-run -C <id>）",
      "body_file":            "本文の決定論的コピー先パス（純本文を読むならこのファイル）",
      "artifacts":            ["生成画像の絶対パス", ...],
      "model":                "実際に使用したモデル（-m 指定時）",
      "dropped_models":       ["@ 参照で agy models に無く除外されたモデル", ...], // @ 参照時のみ
      "warnings":             ["非致命の警告（workspace 不一致 / RUN_COMMAND 失敗 / レート制限検出）", ...],
      "run_command_failures": 0,    // agy の build/test 失敗件数（>0 は自己申告要再検証）
      "net_log":              "/path/to/net-access.jsonl",  // sandbox 実行時のみ（既定 ON）
      "exit_code":            0,
      "error":                null   // 失敗時のみ {"reason": ..., "detail": ...}
    }
    例: agy-run "質問" 2>/dev/null | jq -r .body             # 本文だけ取り出す
        agy-run "質問" 2>/dev/null | jq -r .conversation_id  # 継続用 ID を取る
        agy-run -C <id> "続きの質問"                          # 同じ会話に追撃
        agy-run -m "@impl" -t 120s "軽い質問"                 # models.json の連鎖を参照
        agy-run -m "gemini-3.8-flash-high" -t 120s "軽い質問" # モデル直接指定
        agy-run "質問" 2>/dev/null | jq -r '.artifacts[]'    # 生成画像のパス列挙
        agy-run "質問" 2>/dev/null | jq -r '.warnings[]?'    # 警告を列挙

exit code:
    0=成功 / 2=引数エラー（unknown_model / invalid_model_alias / models_file_error / unknown_model_alias / no_available_model 等） /
    4=スロットロック待ちタイムアウト /
    42=レート制限（本文ゼロ＋ログに 429）/ 43=成果物生成失敗（成果物ツール呼出あり＋artifactsゼロ）/
    44=アイドルストール（heartbeat 途絶）/ 45=認証要求（非対話実行中に interactive OAuth 要求）/
    124=絶対上限タイムアウト /
    129/130/143=agy-run 自身が SIGHUP/SIGINT/SIGTERM を受信して中断（cancelled_by_signal）/
    その他=agy 自身の exit code
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
