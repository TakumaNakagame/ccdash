# 0011 — collector が自分で ccdash を更新する

- Date: 2026-11-26
- Status: Accepted (2026-11-26、たくまの OK)

## 背景

v1.5.0-rc.7 を各ホストに入れるのに、ホストごとに Claude のセッションを立てて `ccdash update` を打たせた。
ka-nabell Worker（v0.9.61）と Grok Worker では、その Claude のログインが切れていて止まり、ログインし直しの画面（0010）も古い collector なので使えなかった。
更新が Claude に頼っていると、いちばん更新したいときに更新できない。

## 決めたこと

1. **更新は collector 自身がする**（`internal/server/update.go`）。`selfupdate.Run`（決まったリポジトリの GitHub リリース、sha256 を確かめる）で入れ替え、`scheduleRestart`（`{"when":"idle"}` と同じ待ち方）で再起動する。Claude は関わらない。
2. **`GET /api/update`** = `model.UpdateState`（今の版・チャンネル・最新・`available`・`auto`・実行中・入れ替え済みの版・`restart_needed`・最後の結果）。最新の確認は 6 時間に 1 回まで（hub の 4 秒ごとの監視が GitHub に届かないように）。
3. **`POST /api/update {"channel"?}`**: その場で始めて、すぐ返す（クライアントは GET で見る）。選べるのは stable / dev だけで、URL や版は渡せない。hub の許可リストでは attach が要り、ほかのマシン（`tunnel.HeaderPeerID`）からは断る。peer の道・MCP の道具には入れない。
4. **夜の自動更新**: 設定 `auto_update`（off / stable / dev、既定 off）。`updateLoop` が 1 時間ごとに見て、3〜5 時（その機械の時刻）に新しい版があれば 1 晩 1 回だけ試す（`auto_update_last`）。設定はそのマシンでしか変えられない（設定の書き込みは hub の許可リストに無い）。TUI の設定か `ccdash update auto dev`。
5. **自分で再起動できない collector**（TUI に組み込み、`ccdash -k`）は入れ替えまでして `restart_needed` を出す。
6. **画面**: hub の `watchDevice` が `GET /api/update` も読み、`/api/devices` の `update` に載せる。ポータルとアプリの端末の行に「更新 vX」ボタン（確認つき）、「更新中…」「再起動待ち」「再起動が要る」「自動更新」の印。入れ替えたとき・失敗したときにプッシュ（`updateNotice`）。

## 限界

- この機能の無い古い collector は、いちど手で更新する（`ccdash update --channel dev`）。
- GitHub の API の回数制限（認証なしで 1 時間 60 回）に当たると確認が失敗する。`gh auth token` などがあれば使う（`selfupdate.apiToken`）。
