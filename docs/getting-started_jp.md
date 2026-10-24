# はじめての ccdash

初めて ccdash を入れる人向けの手順書です。入れる → 動くのを確かめる → 自分に合う使い方を選ぶ、の順に進めば 15 分ほどで使い始められます。各機能の細かい説明は [使い方ガイド](usage_jp.md) にあります。

> English: [getting-started_en.md](getting-started_en.md)

## ccdash でできること

Claude Code のセッションをたくさん並行して動かしていると、「どれが承認待ちか」「どれが質問して止まっているか」「どれが終わったか」が分からなくなります。ccdash は Claude Code の hook で全セッションを見張り、1 か所にまとめて表示します。承認・返信・再開・新規起動もそこからできます。

入口は 3 つあります。

| 入口 | コマンド | こんなとき |
|---|---|---|
| ターミナル (TUI) | `ccdash` | まずはこれ。全セッションの一覧と、選んだセッションの中身 |
| ブラウザ | `ccdash web`（この PC だけ）/ `ccdash hub`（複数台とスマホ） | チャット画面で返信・承認したい、スマホから見たい |
| MCP | `ccdash mcp`（`ccdash setup` で登録される） | Claude 自身に「私の返事待ちのセッションは？」と聞きたい |

## AI にセットアップしてもらう

Claude Code にセットアップを任せたい場合は、このリポジトリの Claude Code プラグイン（セットアップ用のスキル入り）を入れて頼むのが一番早いです。Claude Code の中で:

```text
/plugin marketplace add TakumaNakagame/ccdash
/plugin install ccdash@ccdash
```

そのあと「ccdash をセットアップして」と頼むと、スキル `ccdash-setup` が環境を調べ、使い方を確認しながら、この文書と同じ手順を進めます。どの設定ファイルを書き換えるかも、書き換える前に説明します。

プラグインを入れたくない場合は、次のように頼んでも同じ手順になります。

```text
https://raw.githubusercontent.com/TakumaNakagame/ccdash/main/plugins/ccdash/skills/ccdash-setup/SKILL.md
を読んで、その手順で ccdash をセットアップして
```

以下は手で入れる場合の手順です。

## 前提

- **OS**: macOS / Linux（amd64・arm64）。Windows では WSL2 の中で動きます（ネイティブの Windows は非対応）
- **Claude Code**: `claude` コマンドが入っていて、ログイン済みであること
- **curl**: hook がイベントを送るのに使います
- あると良いもの: `git`（ブランチ表示・差分レビュー）、`tmux`（TUI からペインへジャンプ）
- **画面の言語**: 日本語と英語に対応しています。ポータルはブラウザの言語に合わせます（⚙ 表示 → 言語 で変更）。TUI と CLI の ccdash 自身の文言は `$LANG` に合わせます（設定 `,` → Language で変更）。`$LANG` が日本語でない環境（`C.UTF-8` など）で日本語にしたい場合は Language を `ja` にしてください

## 1. インストール

```sh
curl -fsSL https://raw.githubusercontent.com/TakumaNakagame/ccdash/main/install.sh | sh
ccdash --version
```

GitHub の最新リリースからバイナリを取ってきて、`PATH` に通った場所（Homebrew のある macOS は `$(brew --prefix)/bin`、Linux は `~/.local/bin`）に置きます。スクリプトの中身を先に読みたい場合は、`curl -fsSLO …/install.sh` で落としてから `sh install.sh` を実行してください。`CCDASH_INSTALL_DIR=/path` で置き場所を変えられます。

`failed to resolve latest release tag` と出たら、GitHub API の匿名レート制限です。`gh auth login` 済みなら自動でそのトークンを使います。そうでなければ `CCDASH_VERSION=v1.0.0` のようにバージョンを指定してください。

## 2. Claude Code とつなぐ

```sh
ccdash setup
```

何度実行しても同じ結果になります。書き換えるのは次の 3 か所で、既存の設定は消しません。

| ファイル | 追加されるもの |
|---|---|
| `~/.claude/settings.json` | ccdash 用の hook（あなたの hook はそのまま）と statusLine の中継（元の statusLine コマンドはそのまま実行される） |
| `~/.claude.json` | MCP サーバー `ccdash`（user スコープ） |
| `~/.claude/settings.json` の `permissions.allow` | `mcp__ccdash__set_status`（毎ターンの状態申告で確認が出ないように） |

ほかに `~/.local/state/ccdash/` に DB・トークン・hook 転送スクリプトができます。hook の送り先は `127.0.0.1` だけで、外には何も送りません。

**すでに動いている claude セッションは、一度終了して再開してください。** hook と MCP は起動時に読み込まれます。

## 3. 動くのを確かめる

```sh
ccdash            # ターミナル 1: ダッシュボード
claude            # ターミナル 2: いつも通り使う
```

ターミナル 2 で何か頼むと、数秒でターミナル 1 の一覧にセッションが出ます。出ない場合は:

- `ccdash sessions`（一覧をテキストで出す）にも出ないなら、hook が届いていません。`ccdash setup` をもう一度実行し、claude を再起動します
- `curl -s http://127.0.0.1:9123/healthz` が応答しないなら、collector が動いていません（下の「常駐させる」）
- ログは `~/.local/state/ccdash/ccdash.log`（TUI に埋め込まれた collector の場合）です

TUI の基本操作は `j`/`k` で移動、`enter` で開く、`n` で新しいセッション、`?` でキー一覧、`q` で終了です。

## 4. 使い方を選ぶ

| やりたいこと | 選ぶもの | 手順 |
|---|---|---|
| ターミナルだけで十分 | TUI ＋ 常駐 collector | [A](#a-ターミナルだけで使う) |
| この PC をブラウザでも操作したい（会社 PC など、外から入れたくない） | ローカルポータル `ccdash web` | [B](#b-ブラウザで使うこの-pc-だけ) |
| 複数の PC・サーバーをまとめて、スマホからも見たい | hub | [C](#c-複数台とスマホで使うhub) |
| 別マシンの collector をこのターミナルから見たい | リモートモード | [D](#d-別マシンの-collector-を見るリモートモード) |

### A. ターミナルだけで使う

`ccdash` は、TUI を開いている間だけ collector を動かします。閉じている間の出来事も記録したいなら、collector を常駐させます。

```sh
ccdash -k          # 裏で collector を起動してから TUI を開く（TUI を閉じても collector は残る）
```

ログイン時に自動で起動したい場合は、Linux / WSL なら systemd の user unit を使います。

```ini
# ~/.config/systemd/user/ccdash.service
[Unit]
Description=ccdash collector

[Service]
ExecStart=%h/.local/bin/ccdash server
Restart=on-failure

[Install]
WantedBy=default.target
```

```sh
systemctl --user enable --now ccdash.service
```

macOS では launchd を使います。[hub.md の plist の例](hub.md#local-portal-ccdash-web)の `web --foreground --no-open` を `server` に置き換えれば、そのまま使えます。

### B. ブラウザで使う（この PC だけ）

```sh
ccdash web         # http://127.0.0.1:9124 を裏で起動し、ワンタイムのログインリンクを開く
```

チャット画面・承認カード・グリッド・差分レビュー・通知が使えます。待ち受けるのは `127.0.0.1` だけで、外部のサーバーは要りません。collector が動いていなければ `ccdash web` が一緒に動かします。

- 別のブラウザで開くときや Cookie が切れたときは `ccdash web open` を実行します
- `ccdash web status` / `restart` / `stop` で管理します
- ログイン時に自動で起動するには、macOS は [launchd の例](hub.md#local-portal-ccdash-web)を使います。Linux は A の unit の `ExecStart` を `%h/.local/bin/ccdash web --foreground --no-open` にします

### C. 複数台とスマホで使う（hub）

自宅サーバーなどに hub（Web ポータル）を 1 つ立て、各マシンからそこへつなぎます。マシン側は外へ接続しに行くだけで、ポートは開けません。

1. hub を立てる: [Hub の立て方](hub.md#deploying-the-hub-step-by-step)（Docker Compose ＋ リバースプロキシ ＋ OIDC か Tailscale）
2. ポータルの「端末を追加」でトークンを発行し、各マシンで次を実行する:
   ```sh
   ccdash hub join --url https://ccdash.example.net   # トークンを貼る
   ccdash server                                      # A と同じように常駐させる
   ```
3. スマホでは、ポータルを「ホーム画面に追加」するとアプリとして使え、通知も届きます

hub は **1 人用** です。`--allowed-email` には自分のアドレスだけを入れてください。ログインできる人は、つないだ全マシンでプロンプトを送り、承認し、セッションを起動できます。

### D. 別マシンの collector を見る（リモートモード）

LAN や VPN 越しに、別のマシンの collector を TUI で見ます。[remote-mode.md](remote-mode.md) を参照してください。ブラウザで足りるなら C の方が簡単です。

## 最初に知っておくと困らないこと

- **ターミナルの権限確認が最大 25 秒遅れて出ることがあります。** collector が動いている間は、PermissionRequest の hook が ccdash の画面で許可／拒否されるのを最大 25 秒待ちます。ccdash で決めなければ、そのあといつものダイアログが出ます。気になる場合は、設定（TUI で `,`）の **Approval blocking** を OFF にしてください
- **中身の操作ができるのは、ccdash が起動したセッションです。** TUI の `n`、ポータルの「新しいセッション」、`resume` で起動したものは、右ペインやブラウザから直接入力できます。普通のターミナルで起動した claude は、ccdash から見ることと承認はできますが、右ペインやブラウザから直接打ち込むことはできません。止まったセッションは、ポータルから返信すると ccdash の中で再開します
- **タイトルは自動で生成されます。** `claude -p` を 1 回呼んで（秘密情報らしい文字列は伏せたうえで）短いタイトルを作るので、少しだけ利用枠を使います。止めたい場合は設定の **Auto-generate titles**（自動生成だけ止める）か **Title generation via claude -p**（`claude -p` を一切使わない）を OFF にします
- **状態申告。** `ccdash setup` のあと、各セッションはターンの終わりに「回答待ち／作業待ち／報告／自動で続行」を申告するようになります。前の 2 つは「要対応」として目立つ表示になります
- **7 日触っていないセッションは自動でアーカイブされます**（ピン留め・プロジェクト所属・実行中のものは除く）。`X` でアーカイブ一覧を見られます。日数は設定の `auto_archive_days` で変えられます（0 で無効）
- **新しいフォルダの「このフォルダを信頼しますか？」で止まらないようにするには、** `ccdash trust ~/src` のように、よく使うフォルダを信頼済みに登録します

## 会社の PC で使うとき

- 外への通信は次のものだけです: `ccdash update` と TUI の更新チェック（GitHub）、タイトル生成（あなたの `claude` 経由）、ポータルの通知を ON にしたときの Web Push（ブラウザのプッシュサービス経由、内容は暗号化）。hub を使わなければ、セッションの中身が外に出ることはありません
- 機能を絞りたい場合は、設定（`,`）の **Apply secure preset** で、承認の待ち受け・タイトル生成・アタッチ・settings.json の自動書き換え・hub 接続をまとめて OFF にできます
- プロキシの内側では、`ccdash web` / `ccdash server` に `HTTPS_PROXY` を渡します
- ccdash は Anthropic 公式のツールではありません。会社の Claude の利用規程と、[Anthropic の規約](https://code.claude.com/docs/en/legal-and-compliance) を確認してください。ccdash は Claude の認証情報を読んだり保存したりしません

## 元に戻す

```sh
ccdash uninstall-hooks                 # hook と statusLine の中継を外す（あなたの hook は残る）
claude mcp remove --scope user ccdash  # MCP サーバーの登録を外す
rm -rf ~/.local/state/ccdash ~/.config/ccdash   # DB・トークン・ログ・設定
rm "$(command -v ccdash)"              # バイナリ
```

`permissions.allow` に追加した `mcp__ccdash__set_status` は害がないので、そのまま残しても構いません。消す場合は `~/.claude/settings.json` から手で削除してください。

## 次に読む

- [使い方ガイド](usage_jp.md): TUI・ポータル・タスク・MCP のすべての機能
- [Hub](hub.md): Web ポータルの立て方と、できること・できないこと
- [Security](security.md): 何を守っていて、どのスイッチで何が止まるか
- 困ったとき: [使い方ガイドのトラブルシューティング](usage_jp.md#14-トラブルシューティング)、または [Issue](https://github.com/TakumaNakagame/ccdash/issues)
