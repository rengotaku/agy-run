---
adr: 3
title: agy の print timeout をログから検知して exit 46 で返す（print_timeout）
status: accepted
date: 2026-10-21
description: agy の --print-timeout による時間切れが exit 0 で成功と誤認されていた。poll.go の固定行をソース位置タグ付きで判定し、新設の exit 46 (print_timeout) を返す。本文と conversation_id は残して -C での継続を可能にし、-m フォールバックや wait-quota は起動しない。
issues: [60, 61]
tags: [error-contract, exit-code, timeout, log]
---

# ADR 0003: agy の print timeout をログから検知して exit 46 で返す

## 背景

agy が `--print-timeout`（`-t`）で時間切れになっても、agy-run は `exit_code: 0` を返し、呼び出し側からは途中で止まった実行が正常終了に見えていた。

issue #60 の調査で、9/16 以降のログを `index.jsonl` と突き合わせたところ、`print timeout` が出た 92 件のうち 83 件が exit 0 で返っていた。
内訳は次のとおり:
- exit 0 かつ error なし: 34 件
- exit 0 かつ empty_output: 49 件
- 残り 9 件: exit 42（rate_limited）

34 件は途中までの本文が返っており、中には「task-418 の完了を待ちます」のような作業途中の独り言が成功として扱われていた。

原因は `internal/runner/runner.go` が `--print-timeout` を agy に渡すだけで、ログ内の時間切れ発生を検出していなかったこと（`PrintTimeout` は agy への引数とエラー詳細の設定値にしか使われていなかった）。結果 JSON は呼び出し側の判断の起点であるため、時間切れを成功と区別できないと途中までの成果を完了と誤認して先へ進む原因になる。

## 決定

ADR 0001 の区分に従い、ADR 0002 の `auth_required` と同様に、**agy 自身が時間切れ時に出力する固定のソース位置タグ付きログ行を決定論的な証拠として検知し、新設の終了コードを割り当てる。**

- **判定**: agy ログから `poll\.go:\d+\] Print mode: print timeout after \S+ with turn in progress` に一致する行を走査する（行末の `after N polls (printed=N)` は固定せず表記変更を吸収）。本文やプロンプト由来の同名文字列はソース位置タグ（`poll.go:N]`）がないため誤検知しない。
- **長大行の読み捨て**: 1 MiB を超える長大行に当たっても走査が止まらないよう、`bufio.Reader` で長い行を読み捨てて後続の走査を継続する（#52 の `bufio.Scanner` 上限による穴を引き継がない）。
- **複数行一致**: 最初の一致行を採用し、結果を決定論的にする。
- **結果**:
  - `exit_code: 46`（`config.ExitPrintTimeout`）
  - `error.reason: "print_timeout"`
  - `error.detail`: 該当の一致行
  - `error.print_timeout`: 設定値（`cfg.PrintTimeout`）
  - `error.hint`: 「`agy が --print-timeout で止まりました。本文は途中までです。-C <conversation_id> で続きを依頼するか、-t を延ばしてください`」
  - `index.jsonl` の `error_class` に `"print_timeout"` を記録。
- **本文と会話 ID の保持**: 本文（`body`）と `conversation_id` は破棄せずそのまま返す。途中までの成果を捨てず、呼び出し側が `-C <conversation_id>` で続きを依頼できるようにするため。
- **分類の優先順位**: watchdog の強制終了、シグナル受信、`auth_required`、`rate_limited` の後、`artifact_missing` と `empty_output` の前に置く。`r.Error == nil` の場合のみ設定する。
- **42 との共存**: 本文ゼロで 429 のシグネチャもある場合は従来どおり `rate_limited` (42) を返す。本文ありで 429 シグネチャもある場合は 46 を返し、既存のレート制限の兆候は `warnings` に残す。
- **再試行の抑制**: 46 では `-m` のモデルフォールバック連鎖および `--wait-quota` を動かさない（時間切れで次モデルを試すと枠を余計に消費するため）。
- **判定不能時の扱い**: ログが読めないときは何もしない（判定不能を失敗にしない）。

## 検討して捨てた案

- **既存の 124 に寄せる**: 124 は agy-run 側の絶対上限（watchdog `ExitAbsTimeout`）で意味が違う。124 は agy-run が外側から強制 kill した中断だが、print timeout は agy 自身が時間切れで終了したものであり、性質の違いを区別するため新設の 46 とした。
- **本文が空のときだけ 46 にする**: 途中までの本文がある実行が exit 0 で成功に見える問題が解決しない。実測でも 34 件が途中までの本文を返しており、途中成果を成功と誤認させないために本文の有無にかかわらず 46 とする。
- **時間切れ時の自動リトライ**: agy の作業は非冪等（ファイル編集やツール呼び出し等）であり、API の枠も消費するため入れない。続きが必要な場合は呼び出し側が判断して `-C` で明示的に依頼する。

## 変えてよい前提

- 一致行の正規表現。agy 側のログ文言や行番号の変更に追従する（行番号は `\d+` で吸収済み）。
- `error.hint` の文言。

## 壊すと危ない前提

- **exit 42 の意味を変えないこと**: 本文ゼロかつ 429 のシグネチャがある場合は 42 を優先する。呼び出し側は 42 を見て `-m` のフォールバック連鎖と `--wait-quota` を回している。
- **判定不能を失敗にしないこと**: ログが読めないときに失敗扱いにしない（ADR 0001 / ADR 0002 と同じ）。
- **長い行で走査を止めないこと**: 1 MiB 超の単一行に当たっても読み捨てて後続を走査する（#52 の穴を引き継がない）。
