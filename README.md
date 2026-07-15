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

## サンドボックス (`--sandbox` / `--no-sandbox`)

sandbox は **既定 ON**。agy を [bwrap](https://github.com/containers/bubblewrap) でファイルシステム
隔離した中で実行し、実行中の **全 HTTP(S) 通信** を単一の JSONL ストリームへ記録する。
通信の allowlist / 遮断は一切行わない（記録専用）。

opt-out は `--no-sandbox` または `AGY_RUN_SANDBOX=0`（`false` も可）。優先順位は
**フラグ（`--sandbox`/`--no-sandbox`） > `AGY_RUN_SANDBOX` env > 既定(ON)**。

```bash
agy-run "issue #42 を実装して"                       # 既定でサンドボックス実行
agy-run --no-sandbox "軽い質問"                       # 明示的に無効化
AGY_RUN_SANDBOX=0 agy-run -W ~/code/myrepo/main -d "調査してほしい内容..."
```

- **記録先**: `~/.gemini/antigravity-cli/log/net-access.jsonl`（全 run・全並列スロットが同一ファイルに追記。ローテーションなし）
- **記録内容**: 1接続=1行の JSON（`ts` / `run_id` / `method` / `host` / `port` / `bytes_up` / `bytes_down` / `duration_s`。ダイアル失敗時は `error` も付く）
- **結果 JSON**: サンドボックス実行時のみ `net_log` にログファイルの絶対パスが入る
- **bwrap 未導入**: ハード失敗（`exit 2`）にはしない。警告を1件 `result.warnings` に載せた上で、
  そのrunに限り非サンドボックス（FS隔離・通信記録なし）へ自動的に降格して実行する。降格した run は
  `index.jsonl` にも `sandbox:false`・`net_log` 省略で正直に記録される。bubblewrap を入れると次回以降
  サンドボックス（隔離＋記録）が有効になる。

### bind 一覧（最小構成、issue #11 の PoC を踏襲）

読み取り専用: `/usr`（+ `/bin` `/lib` `/lib64` `/sbin` は symlink かディレクトリのまま複製）、`/etc/ssl`、
`/etc/resolv.conf` `/etc/hosts` `/etc/passwd` `/etc/group` `/etc/machine-id`（存在すれば）、`~/.local`
（agy / agy-run 本体）、`~/.gemini/config/*` の **symlink 先の実体パス**（`skills.json` / `rules/` 等。
dotfiles がどこにあっても `EvalSymlinks` で追従する）。

読み書き: `~/.gemini`（認証・conversations・brain・slot ファイル）、D-Bus セッションソケット
（`DBUS_SESSION_BUS_ADDRESS` から解決。agy の OAuth トークンは Secret Service 経由で保存されており、
このソケットを bind しないと再ログイン要求で落ちる）、cwd（`--chdir` も同じパス）。cwd が git worktree
（`.git` がファイル）の場合は、その `gitdir` と `commondir` が worktree の親ディレクトリ配下にあるときに限り
追加で読み書き bind する（symphony 方式。親ディレクトリの外に解決される場合は信用せずスキップする）。

追加の bind が要るときは `AGY_RUN_SANDBOX_RO_BINDS` / `AGY_RUN_SANDBOX_RW_BINDS`（コロン区切り、
存在しないパスは黙ってスキップ）で指定する。

### 記録の制限

- **プロキシ env を無視する通信は記録に載らない**（生 UDP・QUIC・env を読まないプロセス等）。
  そうした通信も **遮断はしない**（要件どおり無制限）ため、完全な捕捉が必要な場合は
  `--unshare-net` + パケットキャプチャ方式への発展が別途必要（本実装は不採用）。
- **D-Bus セッションバスを bind するため、サンドボックス内から D-Bus 経由のホスト操作が可能**
  （隔離の穴）。将来的に `xdg-dbus-proxy --filter` で `org.freedesktop.secrets` のみへ絞れる。

## ビルド

```bash
make build        # bin/agy-run を生成
make ci           # lint + test-cov + build
```

## インストール（dotfiles 連携）

dotfiles の `make linux`（`scripts/build-go-linux.sh`）がこのリポジトリを
`~/.local/share/dotfiles-tools/agy-run` に clone/pull してビルドし、
`~/.local/bin/agy-run` に配置する。
