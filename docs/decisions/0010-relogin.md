# 0010 — Claude Code のログインし直しを ccdash から

- Date: 2026-11-25
- Status: Accepted (2026-11-25、たくまの OK)

## 背景

haruna（host A）のセッション A269 が「Login expired · Please run /login」で止まった。
スマホからは直せなかった。Claude Code のログインは、ブラウザで URL を開いて許可し、出たコードを貼り戻す流れだが、
ccdash の端末の画面では URL が折り返されて、コピーできないことがある。

## 調べたこと（Claude Code 2.1.331）

- **ログインはマシンごと**。資格情報は `~/.claude/.credentials.json`（macOS はキーチェーン）に 1 つだけある。
  `claude auth status --json` は `loggedIn` と `loginExpiresAt`（ログインの期限）を返し、トークンは出さない。
- **1 台でログインし直せば、そのマシンのほかのセッションも直る**。動いている claude は資格情報ファイルの更新時刻を見ていて、変わると読み直す。
  ただし、ログイン切れで止まったターンは自分では再開しない（セッションの中で `/login` を打ったときだけ、止まった依頼を自動でやり直す）。
- **見分け方**
  - hook: `StopFailure` が Stop の代わりに発火し、`error: "authentication_failed"` が付く（ccdash はこれまで入れていなかった）。
  - transcript: 最後のエントリが `{"type":"assistant", …, "error":"authentication_failed", "isApiErrorMessage":true}`、本文は「Login expired · Please run /login」（ほかに「OAuth token revoked」「Not logged in」「Login reached its time limit」も同じ error）。
- **`claude auth login` は端末がなくても動く**。標準出力に `https://claude.ai/oauth/authorize?…` を 1 行で出し、`Paste code here if prompted >` で標準入力からコードを読む。

## 決めたこと

1. **検知**（collector、`internal/server/login.go`）
   - `install-hooks` に StopFailure（`/hooks/stop-failure`）を足す。`authentication_failed` ならそのセッションに attention needs_you、理由 `ログイン切れ`（`model.LoginReason`）。ほかの API エラーは Stop と同じ扱い（done）。
     古い設定は `hookcfg.NeedsResync`（入っていない event がある）で、次の collector の起動時に書き直される。
   - hook のないセッションは discovery が transcript の末尾で見る（`discovery.AuthFailedAt`）。最後のログインより前、または 24 時間より前のエラーは無視する。
   - `claude auth status` を 10 分ごとと、印を付けたとき（1 分に 1 回まで）に聞く。期限が延びていたら、ほかの場所（セッションの中の `/login`、端末）でログインし直したものとして扱う。
   - `GET /api/login` = `model.LoginState`（expired・期限・3 日前の soon・止まったセッション・進行中のログイン）。**URL は入れない**。
2. **ログインの流れ**（collector が `claude auth login --claudeai` を動かす）
   - `POST /api/login/start`: ログインシェル経由で起動し（`attach.ViaShell` / `SafeEnv`、`BROWSER=true`、Setsid）、URL が出るまで最大 30 秒待って返す。1 マシン 1 つ、始め直すと前のものは止める。10 分で打ち切る。
   - `GET /api/login/flow`: そのときの URL（コードを待つ間だけ）。
   - `POST /api/login/code {"id","code"}`: コードの形（`^[A-Za-z0-9._~#=+/-]{8,512}$`）を確かめて、その子プロセスの標準入力へ書く。終わるまで最大 30 秒待ち、`claude auth status` で確かめて、アカウントを返す。
   - `POST /api/login/cancel`。
   - セッションの端末に `/login` を送る方法は採らなかった。画面から URL を読むのが今回つまずいたところで、ccdash の外で動くセッションには使えないため。
3. **そのあと**: 印の付いたセッションのうち ccdash が動かしているものには、伝言と同じ `typeBlocked` の規則で「ログインし直しました。止まったところから続けてください。」を打つ。打つ直前に transcript をもう一度見て、もう続いていたら打たない。
   動かしていないもの、5 分たっても打てないものは、理由を `ログインし直しました: 続きを送ってください` に替えて残す。
4. **安全**
   - URL とコードはログ・DB・通知・トレースに残さない。URL はログインの記録（メモリ）にだけあり、終わったら消す。コードはプロセスへ渡すだけ。
   - hub の許可リスト: `GET /api/login` は読み取り。`/api/login/flow|start|code|cancel` は人の操作として `attach_enabled` が要り、`tunnel.HeaderPeerID` 付き（ほかのマシン）は断る。`peerDeviceRoutes` には `GET /api/login` だけ。読み取りトークン・MCP の道具からは操作できない。
   - attach が ON なら hub はもともと任意のプロンプトで claude を動かせるので、新しい力にはならない。気を付けるのは「別のアカウントでログインさせられる」ことなので、終わったら必ずアカウントを見せる。
5. **画面**
   - hub: `watchDevice` が `GET /api/login` も読み、`/api/devices` の `login`（expired / soon / ログイン中のときだけ）に載せる。プッシュはマシンごとに 1 回（`loginNotice`、URL は `#/login/<device>`）。セッションごとの「ログイン切れ」のプッシュは出さない。
   - ポータル: 上部の帯 `#loginbar`（`paintLoginBar`）→ `loginDialog`（URL の表示・開く・コピー、コードの入力欄、結果）。チャットには `loginCard`。古い collector（`/api/login/start` が無い）では「ccdash を更新するか /login を」と出す。チャットのカードは、collector の印が無くても transcript の最後の返事が「Please run /login」なら出る。
   - Android アプリも同じ（ccdash-android）。
   - MCP: `overview.login_expired`（ホストごと、止まったセッションの ref と url）。
   - TUI: セッション行の理由「ログイン切れ」だけ（ログインの操作はない）。

## 限界

- 既定の設定ディレクトリ（`~/.claude`）のログインだけを扱う。`accounts` で別の `CLAUDE_CONFIG_DIR` を使っているセッションには効かない。
- API キーや Bedrock / Vertex でつないでいるマシンには関係しない（`claude auth status` が loggedIn を返す限り、何も出ない）。
- 各ホストの collector がこの版以上であること。古い collector は hub に `login` を返さないので、帯もプッシュも出ない（チャットのカードだけ出る）。
