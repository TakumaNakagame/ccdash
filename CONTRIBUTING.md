# Contributing

ccdash への貢献を歓迎します。

## バグ報告 / 機能提案

[Issues](https://github.com/TakumaNakagame/ccdash/issues) でお願いします。以下があると追いやすいです。

- **バグ** — 期待した挙動 / 実際の挙動 / 再現手順 / OS・ターミナル・`ccdash --version`
- **機能提案** — 解きたい課題 / 想定する操作フロー

ccdash の前提（単一ユーザー / loopback のみ / hooks ベース）に外れる提案は、まず Issue で方向性を擦り合わせてから PR にしてもらうとお互い時間を無駄にしません。

## Pull Request

1. このリポジトリを fork
2. `main` から feature ブランチを作成（`feature/xxx` / `fix/xxx`）
3. ローカルで動作確認（`go test ./... && go vet ./... && go build ./...`）
4. PR を作成 — 何を / なぜ を本文に書いてください

PR が CI を通れば早めに見ます。

## ローカル開発

```sh
go install ./cmd/ccdash      # ~/go/bin/ccdash に dev ビルド
ccdash --version             # → "dev"
ccdash install-hooks         # ~/.claude/settings.json に hook を書く
ccdash                       # TUI 起動
```

開発中は `ccdash` でなく `~/go/bin/ccdash` のフルパスで動かすと、リリース版 (`~/.local/bin/ccdash` 等) と切り分けられます。

詳しいアーキテクチャと運用ガイドは以下を参照してください。

- [`docs/usage_jp.md`](./docs/usage_jp.md) — エンドユーザー向け使い方ガイド (日本語)
- [`docs/usage_en.md`](./docs/usage_en.md) — same in English
- [`CLAUDE.md`](./CLAUDE.md) — AI 開発支援向けのアーキテクチャブリーフ
- [`task.md`](./task.md) — もともとの要件・進捗メモ

## コーディング規約

- **Go 1.23+** — `gofmt` / `go vet` が通ること
- **テスト** — 既存パッケージの test を壊さないこと、新機能で純粋ロジックがあれば追加 (`internal/transcript` / `internal/redact` 参考)
- **コミットメッセージ** — 1 行目は 60 文字前後で要約、本文に「何を」「なぜ」。日本語・英語どちらでも OK
- **依存追加** — 新しい外部ライブラリを入れるときは PR 本文で理由を書いてもらえると助かります

## DB スキーマ変更時の注意

`internal/db/db.go` の `migrate()` は **既存ユーザーのDBを壊さない** 形で書いてください。

- `CREATE TABLE IF NOT EXISTS` で初期化
- 列追加は `ALTER TABLE ... ADD COLUMN` を別ループに足す
- 移行が必要な値は同じ場所に `INSERT ... SELECT` 等を書いて吸収
- `internal/settings/settings.go` の Load も legacy キーを読めるなら吸収する

ccdash は state を `~/.local/state/ccdash/ccdash.sqlite` に保存しているので、リリース後の破壊的変更はユーザーの手間に直結します。

## セキュリティ問題

ccdash の attack surface（loopback トークン / hook 経由のデータ取り込み / `claude -p` の外部送信など）に関わる問題を見つけた場合、まず非公開で連絡をください — リポジトリオーナーに DM か、信頼できる連絡経路で。一般機能のバグは Issue で OK です。

## AI による開発支援

このプロジェクトは [Claude Code](https://claude.com/claude-code) (Anthropic) の支援を受けて開発されています。コミット履歴の `Co-Authored-By: Claude` はそのため。AI が生成したコードもメンテナーがレビュー・採用した時点で責任を持ちます。PR で AI アシストを使う場合、明記の必要はありませんが、どんな意図・指示で変更したかを説明してもらえると助かります。

## ライセンス

[MIT](./LICENSE)。PR した時点で MIT ライセンスでの提供に同意したとみなします。
