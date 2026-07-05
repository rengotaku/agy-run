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
- **出力契約**: stdout=結果 JSON、stderr=人向けメタのみ

## 使い方

```bash
agy-run "調査してほしい内容..."
agy-run -m "Gemini 3.5 Flash (High)" -t 120s "軽い質問"
agy-run -C <conversation-id> "続きの質問"       # 会話継続
agy-run "質問" 2>/dev/null | jq -r .body        # 本文だけ取り出す
```

詳細なフラグ・環境変数・exit code は `agy-run -h` を参照。

## ビルド

```bash
make build        # bin/agy-run を生成
make ci           # lint + test-cov + build
```

## インストール（dotfiles 連携）

dotfiles の `make linux`（`scripts/build-go-linux.sh`）がこのリポジトリを
`~/.local/share/dotfiles-tools/agy-run` に clone/pull してビルドし、
`~/.local/bin/agy-run` に配置する。
