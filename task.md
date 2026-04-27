# Claude Code セッション管理ダッシュボード 実装タスク

## 目的

複数の Claude Code セッションを同時に実行している状況を可視化・管理するためのローカルダッシュボードを構築する。

以下を一元管理すること:

* セッション一覧（どこで何が動いているか）
* 各セッションのやり取り（プロンプト / tool 実行）
* Permission / Approve Request の検知
* （可能なら）外部から approve / deny

対象はローカル環境のみ。外部公開はしない。

---

## フェーズ構成

### Phase 0: 調査

Claude Code の仕様を正確に理解する。

### Phase 1: 観測ダッシュボード

セッションとイベントの可視化のみ

### Phase 2: 承認フロー

Approve / Deny の制御

---

## Phase 0: 調査タスク

以下を必ず確認し、`docs/research.md` にまとめること。
不明点は推測せず「未確認」と明記する。

### 1. hooks の仕様

* 設定ファイルの場所

  * `~/.claude/settings.json`
  * `.claude/settings.json`
* 優先順位
* 利用可能な hook イベント一覧
* 各イベントの payload schema
* HTTP hook の挙動
* タイムアウト
* ブロッキングか非ブロッキングか
* 戻り値で制御可能か

### 2. PermissionRequest

* hook が存在するか
* approve / deny を返せるか
* 返却フォーマット
* タイムアウト時の挙動
* tool ごとの差分

### 3. transcript

* `transcript_path` の有無
* フォーマット（JSONLか）
* schema
* tail 可能か

### 4. セッション識別

* session_id の有無
* 継続時の扱い
* cwd / repo 情報の有無

### 5. セキュリティ

* repo 内設定の危険性
* hook 実行のリスク
* 安全な運用方法

---

## Phase 1: MVP 実装

### アーキテクチャ

* backend: FastAPI または Node.js
* DB: SQLite
* bind: 127.0.0.1

---

### データモデル

#### sessions

* session_id
* cwd
* repo
* branch
* commit
* first_seen
* last_seen
* status
* transcript_path

#### events

* id
* session_id
* timestamp
* event_type
* tool
* payload_json

#### approvals

* id
* session_id
* timestamp
* tool
* command
* status

---

### hook エンドポイント

以下を実装する:

* /hooks/session-start
* /hooks/user-prompt
* /hooks/pre-tool
* /hooks/post-tool
* /hooks/permission-request
* /hooks/stop

---

### UI

#### セッション一覧

* cwd
* repo
* branch
* 最終更新
* ステータス
* pending approval

#### セッション詳細

* イベント履歴
* tool 実行
* プロンプト

#### approval 一覧

* pending のみ表示

---

## Phase 2: Approve 制御

### 方針

* PermissionRequest を受信
* pending として保存
* 承認結果を返す

### 実装条件

Claude Code が hook の戻り値で制御できる場合のみ実装する。

できない場合:

* 理由を `docs/research.md` に記載
* 観測のみで完了

---

## CLI

以下を提供:

* ccdash server
* ccdash claude
* ccdash sessions
* ccdash approvals
* ccdash install-hooks

---

## ラッパー

ccdash claude は以下を収集:

* cwd
* git 情報
* pid
* tmux 情報

その後 claude を起動

---

## セキュリティ要件

* localhost のみ
* DB permission 0600
* secret マスキング
* repo 設定を信用しない
* audit log を残す

---

## 完了条件

* セッション一覧が見える
* イベントが見える
* approval が検知できる
* 再現可能な setup がある

