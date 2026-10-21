# ADR 索引

<!-- generate-adr-index.zsh による自動生成。手で編集しない -->

| ADR | タイトル | status | date | 要旨 |
|---|---|---|---|---|
| [1](0001-failure-detection-strength-by-evidence.md) | 失敗の検出は証拠の決定性で強さを分ける | accepted | 2026-08-23 | ログの文字列マッチによる判定は warnings に留め、決定論的に判定できるものだけ exit code を割り当てる。 |
| [2](0002-auth-required-detected-from-agy-state-log.md) | 非対話実行での OAuth 要求は agy の状態遷移ログから検知して kill する（auth_required / exit 45） | accepted | 2026-09-08 | keyring に届かず interactive OAuth へ落ちた agy は print-timeout まで待つだけになる。agy 自身が書くソース位置タグ付きの状態遷移行を watchdog で検知し、本文 0 バイトを条件に kill して exit 45 を返す。起動前の D-Bus 到達性判定は false BLOCK と fail-open を同時に増やすため採らない。 |
| [3](0003-print-timeout-detected-from-agy-log.md) | agy の print timeout をログから検知して exit 46 で返す（print_timeout） | accepted | 2026-10-21 | agy の --print-timeout による時間切れが exit 0 で成功と誤認されていた。poll.go の固定行をソース位置タグ付きで判定し、新設の exit 46 (print_timeout) を返す。本文と conversation_id は残して -C での継続を可能にし、-m フォールバックや wait-quota は起動しない。 |
