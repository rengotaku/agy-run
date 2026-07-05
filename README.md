# agy-run

`agy`（Antigravity CLI, Gemini 系）を安全に呼ぶための Go ラッパー CLI。

生の `agy -p` を直接叩くと発生していたハング事故（並行実行スタベーション・
`--print-timeout` 無視・stdin ブロック・kill 直後の exit 144）をラッパー側で潰し、
出力を **1 実行 = stdout に JSON 1 個** の決定論的な契約に固定する。

## 特徴

- **有界並列**: N スロット（既定 3, `AGY_RUN_MAX_CONCURRENCY`）。超過分は空きスロット待ち → `exit 4`
- **多層の強制タイムアウト**: idle watchdog（heartbeat 途絶で `exit 44`）＋ 絶対上限（`exit 124`）
- **stdin ブロック回避**: agy を `</dev/null` で実行
- **モデル名の事前照合**: `-m` が `agy models` と不一致なら実行前に `exit 2`（候補を JSON で返す）
- **作業ディレクトリ固定**: `-W/--workspace` で agy の `cmd.Dir` を指定し、RUN_COMMAND(build/test) の cwd 誤解決を防ぐ
- **モデルフォールバック / quota 待機**: `-m "A,B"` で rate_limited 時に次候補へ自動切替、`--wait-quota` でリセットまで待って再試行
- **workspace 不一致の preflight 警告**: feature worktree から呼んだ差分が trusted workspace(main) に落ちる状況を検出し `warnings` に出力
- **RUN_COMMAND 監査**: agy の build/test 失敗をログから拾い `run_command_failures` / `warnings` に出す（自己申告「テスト緑」の誤信防止）
- **出力契約**: stdout=結果 JSON、stderr=人向けメタのみ

## 使い方

```bash
agy-run "調査してほしい内容..."
agy-run -m "Gemini 3.5 Flash (High)" -t 120s "軽い質問"
agy-run -C <conversation-id> "続きの質問"       # 会話継続
agy-run "質問" 2>/dev/null | jq -r .body        # 本文だけ取り出す

# 実装委譲: 対象リポの main worktree を -W で渡し、フォールバック候補も並べる
agy-run -W ~/code/myrepo -m "Gemini 3.1 Pro (High),Gemini 3.5 Flash (High)" \
        -d "issue #42 を実装して"
agy-run --wait-quota -m "Gemini 3.1 Pro (High)" "quota 復帰を待って続行"
```

詳細なフラグ・環境変数・exit code は `agy-run -h` を参照。

## agy の書き込み先と worktree 併用（重要）

agy は起動 cwd を無視し、`trustedWorkspaces`（`~/.gemini/antigravity-cli/settings.json`）の
ルート（コンテナ構成では `~/code/<repo>` → `<repo>/main`）にリポを解決して **常に `main/` に書き込む**。
そのため feature worktree から呼ぶと、編集差分は呼び出し元の worktree ではなく `main/` に落ちる。

- agy-run は起動前にこの食い違いを検出し、`result.warnings`（と stderr）へ警告を出す。
- `-W <main-worktree>` を渡すと RUN_COMMAND(build/test) の cwd は固定できるが、**agy のファイル編集先は依然 `main/`** である点に注意（agy 側の仕様）。
- 委譲実務では、実装後に `main/` の該当差分を目的の worktree へ手移送する
  （`git -C main diff -- <files>` → `git -C main restore -- <files>` → `git -C <wt> apply <patch>`）。

## ビルド

```bash
make build        # bin/agy-run を生成
make ci           # lint + test-cov + build
```

## インストール（dotfiles 連携）

dotfiles の `make linux`（`scripts/build-go-linux.sh`）がこのリポジトリを
`~/.local/share/dotfiles-tools/agy-run` に clone/pull してビルドし、
`~/.local/bin/agy-run` に配置する。
