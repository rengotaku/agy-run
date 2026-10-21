# agy-run アーキテクチャ構成

## 1. 全体構成

`main.go` と `internal/` 配下のパッケージが担当する機能一覧。

| パッケージ / ファイル | 担当機能 |
| --- | --- |
| `main.go` | エントリポイント。CLI引数と環境変数のパース、ヘルプ表示、ホームディレクトリ取得、`runner.Run` の呼び出し、exit code によるプロセス終了の制御。 |
| `internal/authreq` | 非対話 print mode 中に agy が interactive OAuth に遷移したことをログから検出。 |
| `internal/config` | CLIフラグ（getopts互換）と環境変数の解析、Config構造体の構築、プログラム全体の exit code 定数および各種デフォルト値の定義。 |
| `internal/convlog` | ログファイルからの会話ID抽出、`transcript.jsonl` や `settings.json` からの実効モデル解析、成果物ツール呼び出し回数のカウント、新規画像成果物の検出、成果物ツール連続呼び出しによる空転（tool stall）検出。 |
| `internal/emit` | stdout に出力する単一の JSON オブジェクト (`Result`, `Error`) のエンコードと、stderr への人向け診断メッセージ出力の制御。 |
| `internal/index` | 実行完了ごとに `index.jsonl` へ 1 行の JSON レコードを追記。ファイル権限 (0600) の強制および容量超過時のローテーション（`.1`, `.2`, `.3`）制御。 |
| `internal/printtimeout` | agy が `--print-timeout` で時間切れ終了したことをログから検出。 |
| `internal/ratelimit` | agy ログ内のレート制限/過負荷シグネチャのカウントおよび `Resets in ...` からの再試行待ち時間のパース。 |
| `internal/runcmd` | agy ログから `CORTEX_STEP_TYPE_RUN_COMMAND` (build/test) ステップの失敗メッセージを検出。 |
| `internal/runner` | 全体の実行制御。起動元プロセスグループからの切り離し、スロット取得、クールダウン処理、bwrap / netlog の準備、agy プロセスの起動と watchdog 監視、エラー判定と分類、フォールバック/クォータ待機ループ、結果の emit と index 追記の統括。 |
| `internal/sandbox` | `bwrap` (Bubblewrap) によるファイルシステム隔離引数の構築、`GH_TOKEN` のパイプ経由注入、および HTTP(S) 通信を全記録するローカルフォワードプロキシ (`Proxy`) の提供。 |
| `internal/slot` | スロットごとの flock ファイル (`.agy-run.slot.N`) による有界並列実行セマフォの制御とロック取得待ち処理。 |
| `internal/watchdog` | アイドルタイムアウト (`AGY_RUN_IDLE_LIMIT`) と絶対上限タイムアウト (`AGY_RUN_ABS_LIMIT`) による 2 段階のプロセスキルフラグの監視および `Probe` による早期キル判定。 |
| `internal/workspace` | agy の `settings.json` 内 `trustedWorkspaces` を読み取り、カレントディレクトリが非 trusted で兄弟 worktree が trusted の場合に発生する差分のずれを警告。 |

## 2. 1 回の実行の流れ

各処理フェーズと関与するパッケージの流れ。

1. 引数と環境変数の解析
   `main.go` が `config.Parse` を呼び出し、`internal/config` パッケージが CLI 引数 (`os.Args[1:]`) と環境変数を評価して `config.Config` 構造体を組み立てる。
2. 起動元からの分離と初期設定
   `internal/runner` パッケージの `detachFromLauncher` が呼び出し元のプロセスグループから自プロセスを分離する。状態保存用ディレクトリを作成する。
3. モデル妥当性検証
   `internal/runner` パッケージが `-m` 指定時のモデル名を `agy models` の一覧と照合する。不適合な場合は `ExitUsage` (2) で即座に終了する。
4. クールダウン待機
   `internal/runner` パッケージが前回の kill タイムスタンプ (`.agy-run.last-kill`) を参照し、指定クールダウン時間（30秒）が経過していない場合は残り時間だけ待機する。
5. スロットロック取得
   `internal/slot` パッケージの `Acquire` が `.agy-run.slot.N` に対する `flock` を取得する。指定時間（`-w` 秒）内に空きスロットを獲得できない場合は `ExitLockTimeout` (4) で終了する。
6. サンドボックスとプロキシの起動
   `internal/sandbox` パッケージが `--sandbox` 設定時（既定ON）に `net-access.jsonl` を開き、ローカルプロキシを起動して `bwrap` 引数を生成する。`GH_TOKEN` はパイプ経由でサンドボックス内部へ安全に渡される。
7. agy プロセスの起動と監視
   `internal/runner` パッケージが `bwrap`（または直接 `agy`）を起動し、標準出力を一時ファイルにキャプチャする。同時に `internal/watchdog` パッケージの監視ゴルーチンが動作し、ログ・本文・`transcript.jsonl` の更新日時を定期的に判定する。また `Probe` により非対話中の interactive OAuth 遷移（本文 0 バイトかつ一致行あり）を早期検知する。タイムアウトや認証要求の発生時はプロセスグループ全体を SIGKILL で停止する。
8. 結果の収集とログ解析
   `internal/convlog` パッケージが会話 ID、新規生成画像、実効モデル、ツール空転を判定する。`internal/runcmd` パッケージが build/test の失敗行をスキャンする。`internal/ratelimit` パッケージがレート制限ヒット数とリセット時間を抽出する。`internal/workspace` パッケージが作業ツリーの不一致警告を生成する。
9. 失敗の分類と再試行制御
   `internal/runner` パッケージが規定の優先順位に従ってエラー理由と exit code を決定する（watchdog 判定の直後に自力終了時の `auth_required` 判定、続いて `rate_limited`、`print_timeout`、`artifact_missing` 等）。`rate_limited` の場合は、`-m` の次候補モデルへの切替や `--wait-quota` による待機・再試行ループを実行する（`auth_required` や `print_timeout` はモデル切替を行わない）。
10. 出力とインデックスへの記録
    `internal/index` パッケージが実行レコードを `index.jsonl` へ追記する（必要に応じて非ブロッキング flock によるファイルローテーションを実施）。最後に `internal/emit` パッケージが stdout に単一の JSON オブジェクトを出力する。

## 3. 出力契約

`internal/emit` パッケージで定義される、stdout に出力される単一 JSON オブジェクトの構造。

### Result オブジェクト (stdout)

| フィールド | 型 | 必須/任意 | 意味・内容 |
| --- | --- | --- | --- |
| `body` | string | 必須 | agy が出力した回答本文。 |
| `conversation_id` | string | 任意 (`omitempty`) | 会話継続用の UUID。次回実行時に `-C <id>` で指定可能。 |
| `body_file` | string | 任意 (`omitempty`) | 本文が保存された決定論的コピー先のファイルパス (`<lastBodyDir>/<conv_id>.md`)。 |
| `artifacts` | string 配列 | 任意 (`omitempty`) | 今回の実行で新規生成された画像ファイルなどの絶対パス配列。 |
| `model` | string | 任意 (`omitempty`) | 実際に使用されたモデル（`-m` でフォールバック候補を指定した際に選択されたモデル）。 |
| `warnings` | string 配列 | 任意 (`omitempty`) | 非致死的な警告メッセージの配列（workspace不一致、RUN_COMMAND失敗、レート制限検出等）。 |
| `run_command_failures` | int | 任意 (`omitempty`) | agy の build/test (`RUN_COMMAND`) が失敗した件数。 |
| `net_log` | string | 任意 (`omitempty`) | サンドボックス実行時に通信ログが記録された `net-access.jsonl` の絶対パス。 |
| `exit_code` | int | 必須 | プロセスの終了コード。 |
| `error` | Error オブジェクト | 任意 (`omitempty`) | 実行失敗時のみ設定される詳細エラー構造体。成功時は null。 |

### Error オブジェクト (`error` フィールドの中身)

| フィールド | 型 | 必須/任意 | 意味・内容 |
| --- | --- | --- | --- |
| `reason` | string | 必須 | エラーの分類理由（例: `bad_option`, `missing_prompt`, `rate_limited`, `print_timeout`, `artifact_missing`, `stall`, `timeout`, `lock_timeout`, `unknown_model`, `agy_error`, `empty_output` 等）。 |
| `detail` | string | 任意 (`omitempty`) | 詳細メッセージやスタックトレーステキスト。 |
| `hint` | string | 任意 (`omitempty`) | 対処法のヒント。 |
| `note` | string | 任意 (`omitempty`) | 補足事項。 |
| `available_models` | string 配列 | 任意 (`omitempty`) | 不正なモデル指定時に提示される有効なモデル名一覧。 |
| `models_tried` | string 配列 | 任意 (`omitempty`) | レート制限時に再試行されたモデル候補のリスト。 |
| `waited_sec` | int | 任意 (`omitempty`) | `--wait-quota` でクォータリセットまで待機した秒数。 |
| `log_ratelimit_hits` | int | 任意 (`omitempty`) | ログ内で検出されたレート制限シグネチャの該当件数。 |
| `idle_limit_sec` | int | 任意 (`omitempty`) | アイドルストール判定の閾値（秒）。 |
| `abs_limit_sec` | int | 任意 (`omitempty`) | 絶対上限タイムアウトの閾値（秒）。 |
| `print_timeout` | string | 任意 (`omitempty`) | agy に引き渡された `--print-timeout` の設定値。 |
| `wait_sec` | int | 任意 (`omitempty`) | スロットロック取得の待ち時間設定値（秒）。 |
| `max_concurrency` | int | 任意 (`omitempty`) | 最大並列スロット数。 |

## 4. exit code の一覧と発生条件

`internal/config` パッケージの定数および各判定ロジックに基づく終了コード。

| exit code 定数 | 値 | 発生条件・決定ロジック |
| --- | --- | --- |
| `ExitOK` | 0 | 正常終了。 |
| `ExitUsage` | 2 | CLI引数の構文エラー、プロンプトの指定漏れ、存在しないモデルの指定（`-m`）、ホームディレクトリ取得失敗、または初期化失敗。 |
| `ExitLockTimeout` | 4 | `-w` 秒以内に有界並列スロット (`.agy-run.slot.N`) のロックを取得できなかった場合。 |
| `ExitRateLimited` | 42 | agy のログ内にレート制限/過負荷のシグネチャが存在し、かつ本文 (`body`) が 0 バイトの場合。 |
| `ExitArtifactMissing` | 43 | `rate_limited` でない場合において、`transcript.jsonl` 上で成果物生成ツール (`generate_image`) の呼び出し記録があるにもかかわらず、成果物ファイルが 0 件の場合。 |
| `ExitStall` | 44 | アイドル watchdog が判定。ハートビート (ログ・本文・`transcript.jsonl` の最新 mtime) の更新が `AGY_RUN_IDLE_LIMIT` 秒間途絶えた場合。 |
| `ExitAuthRequired` | 45 | 非対話 print mode 中に agy が interactive OAuth を要求（一致行あり）かつ本文が 0 バイトの場合。実行中の watchdog kill、またはプロセス終了後の分類で発生。 |
| `ExitPrintTimeout` | 46 | agy が `--print-timeout` で時間切れ終了（一致行あり）。agy 自身の exit code にかかわらず設定。本文・conversation_id は保持。 |
| `ExitAbsTimeout` | 124 | 絶対 watchdog が判定。実行開始からの経過時間が `AGY_RUN_ABS_LIMIT` 秒を超過した場合。 |
| - | 128+シグナル番号 | agy-run 自身が SIGINT(130) / SIGTERM(143) / SIGHUP(129) を受信し、agy プロセスグループを停止した場合 (`cancelled_by_signal`)。 |
| - | 137 | agy-run はシグナルを受けておらず、agy だけが外から殺された（detail に waitErr）。 |
| (agyのexit code) | その他 | agy プロセスが自発的に返した終了コード。 |

## 5. 失敗の分類の優先順位

`internal/runner/runner.go` の `attempt` 関数におけるエラー分類および判定の実行順序。

1. Watchdog 強制終了判定 (`Stall` / `AbsTimeout` / `AuthRequired`)
   `watchdog.Reason` が `Stall` の場合は `ExitStall` (44), `Reason: "stall"`。`AbsTimeout` の場合は `ExitAbsTimeout` (124), `Reason: "timeout"`。`AuthRequired` の場合は `ExitAuthRequired` (45), `Reason: "auth_required"` とし、ただちに処理を復帰する。
2. シグナル受信判定 (`cancelled_by_signal`)
   agy-run 自身が SIGINT / SIGTERM / SIGHUP を受けて agy プロセスグループを停止した場合に判定する。`rc = 128 + シグナル番号`（SIGINT=130, SIGTERM=143, SIGHUP=129）、`Reason: "cancelled_by_signal"`、`Detail: シグナル名`（`SIGINT` / `SIGTERM` / `SIGHUP`、その他は `signal <番号>`）とし、ただちに処理を復帰する。モデル fallback や `--wait-quota` 再試行は行わず、以降の auth_required / rate_limited / artifact_missing / empty_output の分類は行わない。
3. 認証要求判定 (`auth_required`)
   ログ内に `printmode\.go:\d+\] Print mode: triggering interactive OAuth$` に一致する行が存在し、かつ本文が 0 バイトであれば `rc = ExitAuthRequired` (45), `Reason: "auth_required"` を設定する。本文が存在する場合は `exit_code` や `r.Error` を変更せず、`warnings` に記録する。
4. レート制限判定 (`rate_limited`)
   エラーが未設定の場合に判定する。ログ内に `429|overloaded|rate.?limit|resource_exhausted|quota` に一致する行が存在し、かつ本文が 0 バイトであれば `rc = ExitRateLimited` (42), `Reason: "rate_limited"` を設定する。本文が存在する場合は `exit_code` や `r.Error` を変更せず、`warnings` にヒット件数を記録する。
5. タイムアウト判定 (`print_timeout`)
   エラーが未設定の場合に判定する。ログ内に `poll\.go:\d+\] Print mode: print timeout after \S+ with turn in progress` に一致する行が存在すれば `rc = ExitPrintTimeout` (46), `Reason: "print_timeout"`, `Detail: 該当行`, `PrintTimeout: cfg.PrintTimeout`, `Hint: 「agy が --print-timeout で止まりました。本文は途中までです。-C <conversation_id> で続きを依頼するか、-t を延ばしてください」` を設定する。agy 自身の終了コードにかかわらず rc は 46 固定となり、本文や conversation_id は破棄せず保持する。モデル fallback や `--wait-quota` は動作しない。
6. 成果物生成失敗判定 (`artifact_missing`)
   エラーが未設定の場合に判定する。`transcript.jsonl` を解析し、実行開始時刻以降の成果物生成ツール (`generate_image`) 呼び出し数が 1 以上で、かつ検出成果物数が 0 の場合に `rc = ExitArtifactMissing` (43), `Reason: "artifact_missing"` を設定する。
7. 空出力判定 (`empty_output`)
   本文が 0 バイト、`rc == ExitOK` (0)、かつエラーが未設定の場合に `Reason: "empty_output"` を設定する。終了コード (`rc`) は 0 のまま維持する。
8. 汎用エラー判定 (`agy_error`)
   `rc != ExitOK` (0) かつエラーが未設定の場合に `Reason: "agy_error"` を設定する（外から agy だけが殺された `rc == 137` の場合は `Detail` に `waitErr.Error()` が入る）。

## 6. transcript を読む機能 (internal/convlog)

`internal/convlog` パッケージが解析するファイルとそれぞれの判定用途。

### 解析対象ファイル

- `agy-run-*.log` (agy の stdout/stderr ログ)
  `ResolveConvID` 関数がログ内の `conversation=<uuid>` または `Created conversation <uuid>` の正規表現から会話 ID を抽出する。
- `transcript.jsonl` (`brain/<conv_id>/.system_generated/logs/transcript.jsonl`)
  `ExtractModelFromTranscript` 関数が `USER_SETTINGS_CHANGE` イベント内の `Model Selection`（例: `The user changed setting Model Selection from ... to <ModelName>`）を抽出し、使用されたモデルの表示名を取得する。
  `CountArtifactToolCalls` 関数が実行開始マーカー以降の `tool_calls` のうち、成果物生成ツール (`generate_image`) の呼び出し回数をカウントする。
  `DetectToolStall` 関数が実行開始マーカー以降のツール呼び出し履歴を追跡し、成果物を作る同一ツール（`schedule` 除外、非成果物ツールが挟まると連続リセット）が閾値 (`AGY_RUN_TOOL_REPEAT_LIMIT`、既定10) 以上連続して実行され成果物が 0 件の場合に空転警告文字列を生成する。
- `settings.json` (`~/.gemini/antigravity-cli/settings.json`)
  `ExtractModelFromSettings` 関数が `"model"` フィールドを取得する。

### 判定用途と実効モデル優先順位

- 実効モデルの決定 (`ResolveEffectiveModel`):
  1. CLIフラグ `-m`
  2. `transcript.jsonl` の `Model Selection` (可能であれば kebab-case ID へ正規化)
  3. `settings.json` の `model` (可能であれば kebab-case ID へ正規化)
  4. 判定不能時は `(unresolved)`
- 成果物欠損判定 (`CheckArtifactMissing`):
  成果物ツール呼び出し数 > 0 かつ 成果物数 == 0 のときに `artifact_missing` (exit 43) を返却する。
- 新規成果物検出 (`NewImageArtifacts`):
  `brain/<conv_id>/` 配下の画像を走査し、実行開始時刻以降に更新された画像パス一覧を取得する。

## 7. 環境変数の一覧

`internal/config` パッケージおよび関連パッケージで参照される環境変数一覧。

| 環境変数 | 既定値 | 効果・機能 |
| --- | --- | --- |
| `AGY_RUN_MAX_CONCURRENCY` | 3 | 有界並列スロット数。1未満を指定した場合は1へ補正。 |
| `AGY_RUN_IDLE_LIMIT` | 600 | アイドル監視の上限秒数 (10分)。ハートビート停止で exit 44。 |
| `AGY_RUN_ABS_LIMIT` | 3600 | 絶対壁時計時間上限秒数 (60分)。超過で exit 124。 |
| `AGY_RUN_POLL` | 20 | watchdog のポーリング間隔秒数。1未満は1へ補正。 |
| `AGY_RUN_MAX_QUOTA_WAIT` | 14400 | `--wait-quota` 指定時の待機上限秒数 (4時間)。 |
| `AGY_RUN_TOOL_REPEAT_LIMIT` | 10 | 成果物ツールの同一連続呼び出しによる空転検出の閾値回数。 |
| `AGY_RUN_SANDBOX` | 1 (ON) | サンドボックスの有効化/無効化。`0` または `false` で無効化。CLIフラグ (`--sandbox`/`--no-sandbox`) が優先。 |
| `AGY_RUN_SANDBOX_RO_BINDS` | (空) | サンドボックス内に追加する読み取り専用 bind パス一覧（コロン区切り）。 |
| `AGY_RUN_SANDBOX_RW_BINDS` | (空) | サンドボックス内に追加する書き込み可能 bind パス一覧（コロン区切り）。 |
| `AGY_RUN_SANDBOX_NO_DBUS` | (空) | `1`, `true`, `yes`, `on` で D-Bus セッションソケットの bind を停止。エスケープ経路遮断用。 |
| `AGY_RUN_INDEX_MAX_BYTES` | 10485760 (10MB) | `index.jsonl` のローテーション閾値バイト数。0 でローテーション無効。 |
| `GH_TOKEN` / `GITHUB_TOKEN` | (空) | ホストの GitHub 認証トークン。サンドボックス内へパイプ経由で安全に再注入される。 |

## 8. テストの構成

テストファイルと fixture の配置場所、および防衛対象。

### fixture ファイル (`internal/convlog/testdata/`)

| ファイル名 | 保持する防衛・テスト対象 |
| --- | --- |
| `transcript_malformed.jsonl` | 不正または中途半端に壊れた JSONL 行が含まれていても、パニックを起こさずにパースをスキップ/復旧できることを保障。 |
| `transcript_other_tools.jsonl` | 成果物生成以外のツール（`find_by_name`, `read_file` 等）のみが含まれる場合に `CountArtifactToolCalls` が誤ってカウントを増やさないことを保障。 |
| `transcript_quoted_marker.jsonl` | 会話本文や思考ログ内の引用テキストにある `<USER_SETTINGS_CHANGE>` マーカーに誤反応せず、実際のモデル変更イベントのみを抽出することを保障。 |
| `transcript_real_marker.jsonl` | 実際の `Model Selection` 変更ログ形式から正確にモデル名を抽出し、kebab-case ID に正規化できることを保障。 |
| `transcript_text_only.jsonl` | ツール呼び出しを含まない通常のテキスト応答ログに対する正常動作を保障。 |
| `transcript_with_image.jsonl` | `generate_image` 呼び出しを含む正常なログに対して成果物ツールのカウントが正確に行われることを保障。 |

### テストファイルと防衛対象

| テストファイル | 防衛・検証対象 |
| --- | --- |
| `internal/authreq/authreq_test.go` | 非対話時の interactive OAuth 要求行の検出、ソース位置タグなし文字列の誤検知防止、ログ未存在時の安全性を保障。 |
| `internal/config/config_test.go` | 引数パース、単位なし秒数補完、フォールバックモデル一覧展開、サンドボックス優先度（フラグ > 環境変数 > 既定）の互換性を保障。 |
| `internal/convlog/convlog_test.go` | 会話ID抽出優先度、実効モデル決定順序、成果物欠損判定 (`artifact_missing`)、継続実行時の時刻フィルタリング、ツール空転判定を保障。 |
| `internal/emit/emit_test.go` | stdout に出力される単一 JSON オブジェクトの構造およびフォーマット契約を保障。 |
| `internal/index/index_test.go` | `index.jsonl` のパーミッション (0600)、容量上限ローテーション、非ブロッキング flock による並行書き込み安全性を保障。 |
| `internal/ratelimit/ratelimit_test.go` | レート制限キーワードの正規表現マッチ、および `Resets in` の待機時間パース処理を保障。 |
| `internal/runcmd/runcmd_test.go` | agy の `RUN_COMMAND` 失敗キーワード検出によるテスト誤判定の防止機能を保障。 |
| `internal/runner/runner_test.go` | 実行全体の統合制御、モデル検証、クールダウン、スロット取得、watchdog 連携、exit code 変換を保障。 |
| `internal/runner/sandbox_test.go` | bwrap 非存在時の警告降格動作およびオプション解決を保障。 |
| `internal/runner/detach_linux_test.go` | 起動元（Claudeなど）からのプロセスグループ分離 (`detachFromLauncher`) を検証し、親の kill による巻き込み中断を防止。 |
| `internal/runner/ghtoken_pipe_integration_test.go` | `GH_TOKEN` を cmdline 引数に公開せず fd パイプ経由で安全注入する機構を保障。 |
| `internal/runner/signal_test.go` | SIGINT/SIGTERM 受信時に agy プロセスグループ全体を連動して kill し、プロセス孤立を防止することを保障。 |
| `internal/sandbox/bwrap_test.go` | bwrap 引数組み立て、ワンショット型トークン削除スクリプト、D-Bus/git-worktree/RO/RW bind の正確性を保障。 |
| `internal/sandbox/netlog_test.go` | プロキシ (`Proxy`) の CONNECT トンネル/HTTP 中継動作および `net-access.jsonl` への直列化追記を保障。 |
| `internal/slot/slot_test.go` | 並行スロットロック (`flock`) の排他制御、タイムアウト、プロセス終了時の自動アンロックを保障。 |
| `internal/watchdog/watchdog_test.go` | アイドルタイムアウト (`Stall`) と絶対タイムアウト (`AbsTimeout`) の順序制御およびハートビート監視を保障。 |
| `internal/workspace/workspace_test.go` | agy の書き込み先が兄弟 worktree にずれる現象の事前警告検出を保障。 |
