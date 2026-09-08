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
- **非空本文でのレート制限警告**: 本文が返っているがログに 429（レート制限）の兆候がある場合、`exit_code` や `error` は変更せず `warnings` に記録する（調査タスク等での誤検知による中断を防ぎつつレート制限を可視化）
- **同一ツールの空転検出**: 成果物が生成されないまま同一ツールが繰り返し呼び出された場合（既定 10 回, `AGY_RUN_TOOL_REPEAT_LIMIT`）、`warnings` に空転時間とともに記録する（実行は止めない）
- **成果物欠落検知**: 成果物生成ツール（`generate_image`）を実行したのに artifacts が 0 件の場合は `artifact_missing` として失敗扱い（`exit 43`）
- **認証要求の即時検知**: 非対話 print mode 中に interactive OAuth 要求が出た場合（D-Bus 不達等）、タイムアウトまで待たずに即 kill → `exit 45` (`auth_required`)
- **実効モデルの記録**: `index.jsonl` の `model` には「`-m` 指定時はその値 / 解決できれば kebab-case ID / 対応表に無ければ表示名のまま / どれも取れなければ `(unresolved)`」を記録

- **出力契約**: stdout=結果 JSON、stderr=人向けメタのみ


## 使い方

```bash
agy-run "調査してほしい内容..."
agy-run -m "gemini-3.6-flash-high" -t 120s "軽い質問"
agy-run -C <conversation-id> "続きの質問"       # 会話継続
agy-run "質問" 2>/dev/null | jq -r .body        # 本文だけ取り出す

# 実装委譲: 対象リポの main worktree を -W で渡し、フォールバック候補も並べる
agy-run -W ~/code/myrepo -m "gemini-3.1-pro-high,gemini-3.6-flash-high" \
        -d "issue #42 を実装して"
agy-run --wait-quota -m "gemini-3.1-pro-high" "quota 復帰を待って続行"
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

host の `GH_TOKEN`（無ければ `GITHUB_TOKEN`）があれば、サンドボックス内の `gh` / `git` の
GitHub 認証が通るよう転送する。`~/.config/gh`（human の OAuth admin トークン）は bind しない。
scoped PAT（`GH_TOKEN`）のみを転送する方針。

**転送経路（issue #25）**: トークンの実値は bwrap の起動引数（`--setenv`）には一切載せない。
bwrap プロセスの argv は `/proc/<pid>/cmdline` として同一ユーザーの他プロセスから
`ps auxww` / `pgrep -af bwrap` で読めてしまうため、`--setenv GH_TOKEN <値>` は事実上の平文
漏洩経路になる。代わりに次の経路で転送する:

1. agy-run が `os.Pipe()` を作り、write 側にトークン値を書き込んで close する
2. read 側を `exec.Cmd.ExtraFiles` に積む（子プロセスから見て fd 3 になる）
3. bwrap には `--perms 0600 --file 3 /tmp/.agy-run-gh-token` を渡す（fd の中身を mode 0600 の
   **通常ファイル**としてサンドボックス内の tmpfs に書き込む bwrap 標準機能。bwrap の argv に
   現れるのは fd 番号と固定パスのみで、トークンの値ではない。`--ro-bind-data` は使わない —
   これは bind mount を作る flag で、mount point を `rm`/`unlink` すると ro/rw を問わず常に
   `EBUSY (Device or resource busy)` になり、次の手順4の one-shot 削除が失敗するため）
4. bwrap の `--` 以降は agy バイナリを直接 exec せず、小さな `/bin/sh -c` ラッパーを挟む:
   `export GH_TOKEN="$(cat /tmp/.agy-run-gh-token)"; rm -f /tmp/.agy-run-gh-token; exec "$0" "$@"`
   （`"$0" "$@"` に元の argv をそのまま positional parameter として渡すので、シェル解釈による
   インジェクションの余地はない）

GH_TOKEN が無い（未設定）場合はこのラッパーを挟まず、従来どおり agy バイナリを直接 exec する。
なお agy プロセス自身の環境変数（`/proc/<agy-pid>/environ`）には最終的に `GH_TOKEN` が残る
（`gh`/`git` に環境変数として渡す必要があるため）。`ps`/`pgrep -af` のような日常的な観測手段
には載らなくなるが、同一ユーザーの別プロセスから `/proc/pid/environ` を意図的に読む行為までは
防げない（プロセス分離・別 uid 化は別 issue のスコープ）。

### 記録の制限

- **プロキシ env を無視する通信は記録に載らない**（生 UDP・QUIC・env を読まないプロセス等）。
  そうした通信も **遮断はしない**（要件どおり無制限）ため、完全な捕捉が必要な場合は
  `--unshare-net` + パケットキャプチャ方式への発展が別途必要（本実装は不採用）。
- **D-Bus セッションバスを bind するため、サンドボックス内から D-Bus 経由のホスト操作が可能**
  （隔離の穴）。将来的に `xdg-dbus-proxy --filter` で `org.freedesktop.secrets` のみへ絞れる。
  当面の緩和策として、`AGY_RUN_SANDBOX_NO_DBUS=1`（1/true/yes/on）を付けると D-Bus セッション
  ソケットを bind しない。issue #19（`-d` 実行時のモデルによる `systemd-run --user` escape）を
  塞ぐが、Secret Service に OAuth トークンがある agy 実行は `Authentication required` で落ちる
  トレードオフがある（issue #11）。Secret Service に依存しない環境（トークンが `~/.gemini` 直下に
  平文で保存されている等）で opt-in する。

## ビルド

```bash
make build        # bin/agy-run を生成
make ci           # lint + test-cov + build
```

## インストール（dotfiles 連携）

dotfiles の `make linux`（`scripts/build-go-linux.sh`）がこのリポジトリを
`~/.local/share/dotfiles-tools/agy-run` に clone/pull してビルドし、
`~/.local/bin/agy-run` に配置する。
