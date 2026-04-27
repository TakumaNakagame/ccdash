# ccdash — 使い方

日常運用向けの実践ガイド。[README](../README.md) はプロジェクト全体の入り口で、こちらは具体的なワークフロー / キーバインド / 各画面の役割に焦点を当てます。

> English: [usage_en.md](./usage_en.md)

---

## 1. 初回セットアップ

最新リリースバイナリをインストールします。

```sh
curl -fsSL https://raw.githubusercontent.com/TakumaNakagame/ccdash/main/install.sh | sh
```

OS / Homebrew の有無に応じて、`PATH` に通っている場所を自動選択します。

| 環境 | デフォルト配置先 |
| --- | --- |
| macOS + Homebrew | `$(brew --prefix)/bin` |
| macOS (Homebrew 無し、`/usr/local/bin` が writable) | `/usr/local/bin` |
| Linux / フォールバック | `~/.local/bin` |

`CCDASH_INSTALL_DIR=/custom/path` で配置先を上書き、`CCDASH_VERSION=v0.1.x` で特定バージョン固定 (GitHub の匿名 API レート制限に当たったときのフォールバック) も可能。

Claude Code に hook を組み込むのは 1 度だけです。

```sh
ccdash install-hooks
```

`~/.claude/settings.json` に ccdash 用の HTTP hook エントリを追記します。冪等なので何度走らせても OK。既存の user hook は `X-Ccdash-Managed: true` マーカーで区別するので壊しません。

バージョンを確認します。

```sh
ccdash --version
```

---

## 2. 日常運用

普段通りに `claude` を使うだけ。ccdash は裏で観測してダッシュボードに集約します。

```sh
claude               # 1 つの tmux pane / ターミナルタブで
ccdash               # 別ペインで TUI 起動。collector も同プロセス内で起動
```

`q` で TUI を閉じると collector も停止。閉じている間に発火した hook event は記録されません。

ダッシュボードを閉じても収集を続けたい場合は次のオプションを使います。

```sh
ccdash -k            # detached collector を起動して TUI を開く。collector
                     # は TUI の生存に依存しない

# もしくは collector を自分で管理:
ccdash server &
ccdash               # 既存サーバを検出してそれを使う
```

`-k` で起動した detached collector のログは `/tmp/ccdash-server.log`。停止は `pkill -f 'ccdash server'`。

---

## 3. ダッシュボードの読み方

```
ccdash                                              sessions: 4   ⚠ pending: 1   12:34:56
─────────────────────────────────────────────────────
  All     home-lab   ccmanage   deploy review        ← タブストリップ
─────────────────────────────────────────────────────
▶ ● 1m   @task.md の内容から、具体的な作業内容を   transcript  (a574854b)
         確認して
         ccmanage:HEAD · a574854b · pid:394903 · ⚠1   USER
                                                       @task.md ...
  ● 10m  @task.md を参照して実装しましょう…           CLAUDE
         home-lab:main · 66eec245 · pid:179833         はい、内容を確認します
                                                       ...
─────────────────────────────────────────────────────
↑/↓ sel  h/l tabs  /search  enter attach  ...         ← キーヒントフッター
```

左ペインがセッション一覧、右ペインが選択中セッションのライブ transcript。タブストリップで repo / operator 命名グループごとに絞り込み。`All` は全件。

### ステータスドット凡例

| 色 | ステータス | 意味 |
| --- | --- | --- |
| 🟢 緑 | `active` | Claude が処理中 (`status: busy`) |
| 🔵 シアン | `idle` | Claude 起動中・入力待ち |
| 🟡 黄 | `recent` | プロセス終了後 6 時間以内 |
| ⚪ グレー | `stopped` | プロセス終了から 6 時間以上 |

### 日付グループ化

`last_seen` でバケット分けされます。

- ★ **Favorites** (固定したものは日付に関係なく上位)
- **Today**
- **Yesterday**
- **This week** (2-7 日前)
- **Earlier this month** (8-30 日)
- `Month YYYY` (それ以上)

`f` でお気に入りトグル。

---

## 4. グループ操作

ストリップにはユニークなグループが全部並びます。アクティブタブはハイライト、`h` / `l` (もしくは `Tab` / `Shift+Tab`) で循環。横幅オーバー時は `‹` `›` 矢印付きでスライドします。

グループの種類は次の通りです。

- **自動派生**: セッションの `s.Repo` または cwd basename。設定ページ (`R` キー) で ON/OFF
- **operator 命名**: セッション選択中に `T` で命名。自動派生より優先。複数セッションに同じ名前を付ければ合流 — 例: `frontend-repo` と `backend-repo` を `feature-x` でまとめる

アクティブグループがアーカイブ等で消えると自動で次のタブへ移ります。

起動時にグループを固定できます。

```sh
ccdash --group home-lab          # ストリップ非表示・固定
ccdash --group "deploy review"   # スペース含む user_group も OK
```

---

## 5. 右ペイン transcript

選択中セッションの最新 USER / CLAUDE / TOOL のやり取りを表示。`~/.claude/projects/<...>/<session_id>.jsonl` を末尾 256 KB だけ tail-read するので、巨大ファイルでも軽快に動きます。

ロール別の背景色は次の通りです。

- **USER** — 暗い青
- **CLAUDE** — 暗い緑
- **TOOL `<name>`** — ティール
- **↳ result** — 暗いグレー
- **↳ ERROR** — 暗い赤

tool 呼び出しと結果は視覚的に結合 (空行無し、結果はインデント深く)。

### スクロール

| キー / 操作 | 効果 |
| --- | --- |
| `Shift+J` / `Shift+K` | 1 行ずつ新しい / 古い方向にスクロール |
| `pgdn` / `pgup` | 半ページずつ |
| マウスホイール (右ペイン上) | 同じ意味でスクロール |
| `Shift+J` で末尾まで戻る | 自動 tail (最新追従) を再開 |

セッション切替で `tailScroll` は 0 にリセット (= 末尾自動追従)。

### 全画面ビューア (`o`)

長く読みたいときは `o` でモーダルビューアに切替。tail-read ではなく**全文ロード**します。

---

## 6. 承認 (Approvals)

デフォルトで `PermissionRequest` hook を最大 25 秒ブロック。Claude の標準プロンプトへフォールバックする前に TUI で判断できます。

セッションに pending があるときの挙動は次の通りです。

- 行が黄色く色づき、`⚠ N pending` バッジ
- 右ペイン下に approval パネル (tool 名 + 入力サマリ)
- ヘッダに合計 `⚠ pending: N`
- 0 → 1 でターミナルベル 1 回

### 判断キー

| キー | 動作 |
| --- | --- |
| `a` | 最古の pending を allow (1 回限り) |
| `A` | allow + セッション中の同種 tool 呼び出しを記憶 (`Bash(git *)` 等) |
| `d` | 最古の pending を deny |

`A` (keep-allow) は `updatedPermissions` を `scope: "session"` で返すため、同等の呼び出しは以降プロンプト無しです。`Bash` の場合は最初の token を glob 化します (`Bash(git status)` → `Bash(git *)`)。

### グループ一括アーカイブ

`Ctrl+X` で現在のタブの全セッションを一括アーカイブします。条件は次の通りです。

1. アクティブフィルタが具体グループ (`All` 不可、誤爆対策)
2. `archive all N sessions in '<tab>'? press 'y'` の確認 (それ以外のキーで cancel)
3. 完了後は次のグループへ自動遷移

アーカイブ view (`X` トグル中) では `Ctrl+X` が**一括解除**になります。

---

## 7. サマリ (`s`)

`s` で選択中セッションの会話を Claude に要約させます。ccdash の処理の流れは次の通りです。

1. ダイジェスト構築 (USER prompt + CLAUDE 返答 + TOOL 呼び出しのみ、tool_result と thinking はノイズ多いので除外)
2. `internal/redact` でシークレットパターンをマスク
3. `claude -p` を隔離サブプロセスで起動

最初の `s` で `y/n` 確認バナーが出ます。確認後の挙動は次の通りです。

- 一覧行に `⏳ summarizing` 表示
- 最大 180 秒 (設定変更可)
- 成功: transcript ストリームの**生成時刻位置**にサマリブロック挿入。新しい activity が来ると古いメッセージと同様に上にスクロール
- 失敗: `✗ summary error` 表示 + エラー文

`claude -p` は `--setting-sources project` + cwd `/tmp` で起動するので、ccdash 自身の hook を継承しません (継承するとサマリ実行が新セッションを生んでループ)。

---

## 8. アタッチ (`enter`)

`enter` でセッションへの移動を試みます。

| セッション状態 | 動作 |
| --- | --- |
| 起動中・tmux pane に居る | `tmux switch-client -t <pane>` |
| 起動中・tmux pane 検出不可 | flash で PID + TTY を表示 (手動切替案内) |
| 停止済み | cwd で `claude --resume <session_id>` |

tmux 連携は自動 (`tmux list-panes` で発見)。tmux 内で `claude` を起動していれば 1 ストロークで pane 切替可能になります。

---

## 9. 検索 (`/`)

`/` を押すとフッター検索入力が開きます。`Enter` を押すと以下のフィールドに対して大文字小文字無視の部分一致でフィルタします。

- `s.DisplayTitle()` (custom title or 自動派生)
- `s.UserGroup`
- `s.Repo`
- `s.Cwd`
- `s.Branch`
- `s.Summary`
- `s.SessionID`

プロジェクトフィルタおよびアーカイブ view と AND 条件で結合。検索アクティブ時はヘッダに `🔍 <query>` 表示。検索入力を閉じた状態で `Esc` を押すとクリア。

---

## 10. 設定ページ (`,`)

`,` で開きます。キーバインドは次の通りです。

| キー | 動作 |
| --- | --- |
| `↑` `↓` / `j` `k` | 行移動 |
| `space` / `enter` | bool トグル / enum 循環 / int 編集 / action 実行 |
| `esc` / `q` / `,` | 一覧に戻る |

設定は DB の `settings` テーブルに永続化。

### リスクのあるトグル

ccdash の介入度合いを「観察のみ」まで下げられます。

- **Approval blocking**: OFF で PermissionRequest ブロックを停止。Claude は標準プロンプトを表示、`a`/`A`/`d` は無効化
- **Summarize via claude -p**: OFF で `s` 無効化、ダイジェストの外部送信ゼロ
- **Attach (enter)**: OFF で `enter` はセッション情報表示のみ、サブプロセス起動無し
- **Auto-rewrite settings.json**: OFF でサーバ起動時の `~/.claude/settings.json` 自動書き換えを停止 (token rotate 時も)

**Apply secure preset** アクションで上 4 つを一括 OFF にして「観察のみ」モードへ。

### レイアウト

- **Vertical layout** (auto / on / off): auto は端末幅から自動判定 (デフォルト)
- **Vertical auto threshold (cols)**: auto モードが vertical へ切り替わる端末幅。デフォルト 100。行に現在の端末幅 (`(now: 142 cols, ≥ threshold)`) も表示
- **Newest at bottom**: 最新を下に並べる (transcript tail と同じ向き)

### チューニング

- **Right-pane tail budget (KB)**: ライブ tail でロードする transcript バイト数。デフォルト 256
- **Summary timeout (s)**: `claude -p` のタイムアウト。デフォルト 180
- **Refresh interval (ms)**: TUI が DB を再クエリする間隔。デフォルト 1000

---

## 11. セルフアップデート

```sh
ccdash update
```

GitHub に最新リリースを問い合わせ、該当アセットをダウンロード、sha256 sidecar 検証後 `os.Rename` で実行中バイナリを差し替え。最新で no-op。

GitHub の匿名 API レート制限 (60/hr) に当たるとエラーメッセージにヒントが出ます。install スクリプトはバージョン明示で逃げられます。

```sh
curl -fsSL https://raw.githubusercontent.com/TakumaNakagame/ccdash/main/install.sh \
  | CCDASH_VERSION=v0.1.3 sh
```

---

## 12. アンインストール

```sh
ccdash uninstall-hooks       # ~/.claude/settings.json から ccdash エントリを削除
rm -rf ~/.local/state/ccdash # DB / token / ログを削除
rm $(which ccdash)           # バイナリ削除
```

`uninstall-hooks` は `X-Ccdash-Managed: true` マーカー付きエントリのみ削除。他の user hook は残ります。

---

## 13. ファイル / ディレクトリ

| パス | 内容 |
| --- | --- |
| `~/.claude/settings.json` | hook エントリ (`install-hooks` が管理) |
| `$XDG_STATE_HOME/ccdash/ccdash.sqlite` | sessions / events / approvals / settings |
| `$XDG_STATE_HOME/ccdash/token` | loopback 共有シークレット (0600) |
| `$XDG_STATE_HOME/ccdash/ccdash.log` | TUI 経由起動の埋め込み collector ログ |
| `/tmp/ccdash-server.log` | detached collector のログ (`-k` モード) |

`$XDG_STATE_HOME` は Linux/macOS で `~/.local/state` がデフォルト。

---

## 14. トラブルシューティング

| 症状 | 原因の見当 |
| --- | --- |
| TUI が空、セッションが出ない | hook 未インストール or token ズレ。`ccdash install-hooks` を再実行 |
| ログに hook event の 401 が並ぶ | token 不一致。`ccdash install-hooks` で再書き込み (もしくは server の auto-sync が次回起動で対応) |
| `claude -p failed: signal: killed` (サマリ) | タイムアウト。設定ページで `summary_timeout_sec` を上げる |
| `vertical_auto_cols` が想定外の幅で切替 | 設定ページで閾値調整 (現在の端末幅が同じ行に表示) |
| `failed to resolve latest release tag` (install/update) | 匿名 API レート制限。`CCDASH_VERSION=v0.1.x` で API スキップ |
| Pending カウントが減らない | 承認の自動 resolve が失敗。discovery loop が 45 秒経過したものを `timeout` に倒すので server を再起動 |

それでも分からない場合、まず埋め込み collector ログ (`~/.local/state/ccdash/ccdash.log`) を見るのが近道。
