# agy-run

`agy`（Antigravity CLI, Gemini 系）を安全に呼ぶための Go ラッパー CLI。

生の `agy -p` を直接叩くと発生していたハング事故（並行実行スタベーション・
`--print-timeout` 無視・stdin ブロック・kill 直後の exit 144）をラッパー側で潰し、
出力を **1 実行 = stdout に JSON 1 個** の決定論的な契約に固定する。

## 特徴

- **有界並列**: N スロット（既定 3, `AGY_RUN_MAX_CONCURRENCY`）。超過分は空きスロット待ち → `exit 4`
- **多層の強制タイムアウト**: idle watchdog（heartbeat 途絶で `exit 44`）＋ 絶対上限（`exit 124`）
- **stdin ブロック回避**: agy を `</dev/null` で実行
- **モデル名の事前照合**: `-m` 直書き時は `agy models` と完全一致を厳格に照合し、不一致なら実行前に `exit 2` (`unknown_model`)
- **モデルエイリアス連鎖（`-m @名前`）**: `models.json` で定義した連鎖を参照。`@` 参照時のみ `agy models` に無い候補を自動除外して残りで実行し、外した名前を `dropped_models` に記録（全滅時は `no_available_model` で `exit 2`）
- **作業ディレクトリ固定**: `-W/--workspace` で agy の `cmd.Dir` を指定し、RUN_COMMAND(build/test) の cwd 誤解決を防ぐ
- **モデルフォールバック / quota 待機**: カンマ区切りの候補や `@名前` の連鎖で rate_limited 時に次候補へ自動切替、`--wait-quota` でリセットまで待って再試行
- **workspace 不一致の preflight 警告**: feature worktree から呼んだ差分が trusted workspace(main) に落ちる状況を検出し `warnings` に出力
- **RUN_COMMAND 監査**: agy の build/test 失敗をログから拾い `run_command_failures` / `warnings` に出す（自己申告「テスト緑」の誤信防止）
- **非空本文でのレート制限警告**: 本文が返っているがログに 429（レート制限）の兆候がある場合、`exit_code` や `error` は変更せず `warnings` に記録する（調査タスク等での誤検知による中断を防ぎつつレート制限を可視化）
- **成果物ツールの空転検出**: 画像成果物が生成されないまま成果物を作るツール（`generate_image`）が繰り返し呼び出された場合（既定 10 回, `AGY_RUN_TOOL_REPEAT_LIMIT`）、`warnings` に空転時間とともに記録する（成果物ツール以外の連続では警告を出さない。実行は止めない）
- **成果物欠落検知**: 成果物生成ツール（`generate_image`）を実行したのに artifacts が 0 件の場合は `artifact_missing` として失敗扱い（`exit 43`）
- **認証要求の即時検知**: 非対話 print mode 中に interactive OAuth 要求が出た場合（D-Bus 不達等）、タイムアウトまで待たずに即 kill → `exit 45` (`auth_required`)
- **print timeout 検知**: agy が `--print-timeout` で時間切れになった場合をログから検出し、`exit 46` (`print_timeout`) を返す。途中までの本文と `conversation_id` は保持され、`-C` で続きを依頼可能
- **実効モデルの記録**: `index.jsonl` の `model` には「`-m` 指定時はその値 / 解決できれば kebab-case ID / 対応表に無ければ表示名のまま / どれも取れなければ `(unresolved)`」を記録

- **出力契約**: stdout=結果 JSON、stderr=人向けメタのみ


## 使い方

```bash
agy-run "調査してほしい内容..."
agy-run -m "@impl" -t 120s "軽い質問"                 # models.json の連鎖を参照
agy-run -m "gemini-3.8-flash-high" -t 120s "軽い質問" # モデル直接指定
agy-run -C <conversation-id> "続きの質問"             # 会話継続
agy-run "質問" 2>/dev/null | jq -r .body              # 本文だけ取り出す

# 実装委譲: 対象リポの main worktree を -W で渡し、連鎖またはフォールバック候補を指定
agy-run -W ~/code/myrepo -m "@impl" -d "issue #42 を実装して"
agy-run -W ~/code/myrepo -m "gemini-3.8-flash-high,gemini-3.1-pro-high" \
        -d "issue #42 を実装して"
agy-run --wait-quota -m "@impl" "quota 復帰を待って続行"
```

詳細なフラグ・環境変数・exit code は `agy-run -h` を参照。

## モデルエイリアス連鎖 (`-m @名前`)

`-m @名前`（名前は `[a-z0-9_-]+`）を指定すると、`models.json` で定義された連鎖順にモデル候補を参照します。

### models.json の配置場所

次の優先順序で探索されます:
1. 環境変数 `AGY_RUN_MODELS_FILE`
2. `$XDG_CONFIG_HOME/agy-run/models.json`
3. `~/.config/agy-run/models.json`

### models.json の形式

```json
{
  "chains": {
    "impl": ["gemini-3.8-flash-high", "gemini-3.1-pro-high"],
    "high": ["gemini-3.8-flash-high"]
  }
}
```
※ `chains` 以外のトップレベルキーは無視されます。

### 動作仕様と候補除外

- **自動除外**: `@` 参照のときだけ、`agy models` の一覧に存在しない候補を自動で外して残りの候補で実行します。
- **dropped_models**: 外された候補は結果 JSON の `dropped_models`（配列）に記録されます（成功時・失敗時問わず出力）。
- **直接指定との違い**: モデル名を直接書いた場合（`-m "gemini-..."`）は照合を緩めず、一覧に無い候補が 1 つでもあれば従来通り `exit 2` (`unknown_model`) で厳しく弾きます。

### exit 2 の reason

モデルエイリアス処理で異常が発生した場合は `exit 2` で即時停止します:
- `invalid_model_alias`: `@` のみ、モデル名直書きとの混在（例: `@impl,gemini-x`）、使用不能文字を含む名前
- `models_file_error`: `models.json` が存在しない・読めない、壊れた JSON、`chains` キーが無い、配列が空、または文字列以外が含まれる
- `unknown_model_alias`: 指定したエイリアス名が `chains` に未定義（hint に定義済みエイリアスをソートして表示）
- `no_available_model`: 連鎖内のすべての候補が `agy models` に存在せず全滅（`available_models` と `dropped_models` を JSON に出力）

## 出力契約 (Result JSON)

stdout には常に 1 つの JSON オブジェクトが出力されます。

```json
{
  "body": "agy の回答本文",
  "conversation_id": "継続用 ID（agy-run -C <id>）",
  "body_file": "本文の決定論的コピー先パス",
  "artifacts": ["生成画像の絶対パス", ...],
  "model": "実際に使用したモデル",
  "dropped_models": ["@ 参照時に agy models に無く除外されたモデル", ...],
  "warnings": ["非致命の警告", ...],
  "run_command_failures": 0,
  "net_log": "/path/to/net-access.jsonl",
  "exit_code": 0,
  "error": null
}
```

### exit code とエラー reason の対応表

| exit code | reason | 意味・発生条件 |
| --- | --- | --- |
| 0 | なし | 正常終了 |
| 2 | `unknown_model`, `invalid_model_alias`, `models_file_error`, `unknown_model_alias`, `no_available_model` 等 | CLI 引数エラー / モデル名・エイリアス不正 |
| 4 | `lock_timeout` | スロットロック待ちタイムアウト（`-w` 秒以内に並列枠を獲得できず） |
| 42 | `rate_limited` | レート制限（本文ゼロかつログに 429 等のシグネチャ） |
| 43 | `artifact_missing` | 成果物生成失敗（成果物生成ツール呼出ありかつ artifacts ゼロ） |
| 44 | `stall` | アイドルストール（ハートビート更新途絶） |
| 45 | `auth_required` | 認証要求（非対話実行中に interactive OAuth 要求） |
| 46 | `print_timeout` | agy の `--print-timeout` による時間切れ（ログに検出行あり。本文と conversation_id は保持） |
| 124 | `timeout` | 絶対上限タイムアウト（`AGY_RUN_ABS_LIMIT` 秒超過） |
| 129 / 130 / 143 | `cancelled_by_signal` | agy-run 自身が SIGHUP / SIGINT / SIGTERM を受信して中断 |
| 137 | `agy_error` | agy プロセスが外部から強制終了された |
| その他 | `agy_error` | agy 自身の非ゼロ終了コード |

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
