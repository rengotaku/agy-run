---
adr: 2
title: 非対話実行での OAuth 要求は agy の状態遷移ログから検知して kill する（auth_required / exit 45）
status: accepted
date: 2026-09-08
description: keyring に届かず interactive OAuth へ落ちた agy は print-timeout まで待つだけになる。agy 自身が書くソース位置タグ付きの状態遷移行を watchdog で検知し、本文 0 バイトを条件に kill して exit 45 を返す。起動前の D-Bus 到達性判定は false BLOCK と fail-open を同時に増やすため採らない。
issues: [50]
tags: [error-contract, exit-code, watchdog, auth, dbus, sandbox]
---

# ADR 0002: 非対話実行での OAuth 要求は agy の状態遷移ログから検知して kill する

## 背景

Claude Code の Bash sandbox 内から agy-run を起動すると、agy は keyring（D-Bus 経由の Secret Service）に届かず token を読めない。その後 print mode なのに interactive OAuth を起動し、ブラウザが開けない環境では `--print-timeout` まで待つだけになる。prompt は送信されず、`index.jsonl` にも残らないので、失敗した事実が後から追えなかった（2026-08-30 から 09-07 の間に 4 回。issue #50）。

agy のログには毎回同じ行が出ている。

```
printmode.go:387] Print mode: triggering interactive OAuth
```

この行は失敗した 4 run すべてに 1 回ずつ現れ、成功した run には 1 回も現れない。

## 決定

**agy が自分の状態遷移として書くソース位置タグ付きの行を、決定論的な証拠として扱い、exit code を割り当てる。**

- 照合は glog のソース位置タグまで含める: `printmode\.go:\d+\] Print mode: triggering interactive OAuth$`。本文や調査プロンプト由来の同名文字列（タグの無い行）は拾わない
- **実行中**: watchdog の poll ごとにログを走査し、一致行があり **かつ本文が 0 バイト** なら process group を kill する（`watchdog.Watcher.Probe` → `AuthRequired`）。stall / timeout と同じ後始末（kill stamp）を通る
- **終了後の分類**: agy が自力で終了した場合も、一致行があり本文 0 バイトなら `auth_required` に分類する。優先順位は watchdog 判定の直後、`rate_limited` の前
- 契約: `exit_code: 45`（`config.ExitAuthRequired`）、`error.reason: "auth_required"`、`error.detail` に一致行、`error.hint` に復旧手段（Claude Code なら `sandbox.excludedCommands` に agy-run を追加、手元なら引数なしで `agy` を起動してログイン）。`index.jsonl` の `error_class` にも `"auth_required"` が入る（他の失敗と同じく `error.reason` をそのまま写す）
- 判定の順序は「一致行の検出 → 本文サイズの確認」。本文サイズを先に見ると、走査中に本文が書かれた場合に古い 0 バイトの観測で kill してしまう（codex レビュー指摘）
- 本文が非空で一致行があるときは kill も分類変更もせず `warnings` に載せるだけ
- `auth_required` は `-m` のモデル fallback を起こさない（exit 42 ではないので既存の分岐で再試行されない。テストで固定）
- 新しい環境変数・opt-out は作らない。D-Bus・sandbox 設定・認証方式に依存しない

## 捨てた案

**起動前に D-Bus session bus の socket へ connect して判定する（issue #50 の初版）。** codex の設計レビューで却下された。D-Bus 到達性は認証可否の必要条件でも十分条件でもない。`AGY_RUN_SANDBOX_NO_DBUS=1` や `~/.gemini` に credentials を置く運用、session bus の無い SSH / cron / systemd 環境を誤って弾く（false BLOCK）。一方で connect が通っても Secret Service 未起動・collection ロック中は防げない（fail-open）。socket path の解決も bwrap の bind 対象抽出器と責務が混ざる。症状そのものを検知する方が両方の誤りが少ない。

**`agy models` の失敗で判定する。** `agy models` の照合は既に best-effort（取れなければ検証を飛ばす）で、失敗理由をログイン切れと特定できない。

**一致行だけで kill する（本文 0 バイト条件を付けない）。** ADR 0001 は「ログの文字列マッチは warnings 止まり」と決めている。今回の行はタグ付きで混入経路が限られるが、本文が出ている実行を止める理由は無いので、本文 0 バイトを条件に足して ADR 0001 と両立させた。

## ADR 0001 との関係

ADR 0001 の「文字列マッチは非決定的」は、本文・検索結果・調査対象から混入しうる `429` / `quota` のような一般語を想定している。今回の一致行は agy の CLI 自身がソース位置タグ付きで書く状態遷移行で、混入経路が無い。この違いを「ソース位置タグまで照合する」「本文 0 バイトを条件に足す」の 2 点で担保し、exit code を割り当てる側に置いた。

## 変えてよい前提

- 一致行の正規表現。agy がログ文言や `printmode.go` の行番号を変えたら追従する（行番号は `\d+` で吸収済み）
- `error.hint` の文言
- poll 間隔（`AGY_RUN_POLL`）。検知の遅れは最大 1 poll 分

## 壊すと危ない前提

- **本文 0 バイト条件**。外すと ADR 0001 の「文字列マッチ単独で exit を動かさない」を破る
- **exit 45 を exit 42 と混ぜないこと**。42 は `-m` fallback と `--wait-quota` が再試行の合図として見ている。認証切れで再試行しても同じ結果になるだけで、枠を消費する
- **分類の優先順位（auth_required → rate_limited）**。逆にすると、ログ内に `quota` 系の語が並ぶ環境で認証切れが rate_limited に化け、呼び出し側が fallback 連鎖を回してしまう
- **判定不能を失敗にしないこと**。ログが読めないときは検知しない（ADR 0001 と同じ）
