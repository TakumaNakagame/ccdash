# 右ペイン仮想ターミナル化 — 検証記録 (vtspike)

- 日付: 2026-09-09
- 目的: tmux に依存せず、右ペインに「本物の Claude Code 画面」を仮想ターミナルとして
  常駐表示できるか。特に **前回 (v0.3.x windowed attach) を葬った 2 つの壁**
  — CJK 列ズレと IME 競合 — が今の部品で解けるかを、本実装前に見極める。
- 成果物: `cmd/vtspike/main.go`（本体未配線の使い捨てプロトタイプ）
- **2026-09-10 追記: 本実装済み。** Bubble Tea v2 移行 + サーバー側エミュレータ
  (`internal/server/screen.go`) + 右ペインのライブ表示 (`internal/tui/live.go`)。
  設計は `docs/decisions/0002-live-right-pane.md`。残るゲートは実端末での IME 目視。
  vtspike は IME 単体確認用ハーネスとして残す。

## 背景 — 前回なぜ撤去したか

`docs/decisions/0001-no-windowed-attach.md` の通り、v0.3.0 で右ペイン埋め込みを
実装したが v0.3.7 で撤去した。撤去前コードを読み直して分かった実際の欠落は 2 点:

1. **実カーソルを置いていなかった。** 旧 `windowed.go` はカーソルを「反転セル」として
   絵に描くだけで、端末の本物のカーソルは Bubble Tea v1 が隠したまま画面末尾に放置。
   IME は本物のカーソル座標に変換中の文字を重ねるため、座標が無い/ズレる状態になり
   幽霊スペースと揺れが出た。決定文書が主因とした「再描画がオーバーレイを潰す」より、
   こちらが本命と判断。
2. **エミュレータ (vt10x) が全角幅を知らなかった。** 1 rune = 1 セルで進むため
   日本語で列がずれた。

## 今回使った部品

| 部品 | バージョン | 役割 |
|---|---|---|
| `charm.land/bubbletea/v2` | v2.0.9 (安定版, 2026-02 に v2.0.0) | View がカーソル座標/形状/可視を返せる |
| `github.com/charmbracelet/x/vt` | v0.0.0-20260906 (pre-v1, タグ無し) | grapheme 幅対応の端末エミュレータ |
| `github.com/charmbracelet/ultraviolet` | v0.0.0-20260703 (bubbletea/x/vt 共通基盤) | Cell/Key/Mouse 型 |
| `github.com/creack/pty` | v1.1.24 (既存) | PTY |

前回との決定的な差:
- **Bubble Tea v2 の `View.Cursor`** で実カーソルを右ペイン座標に置ける
  (`Position{X: x+cur.X, Y: cur.Y}`)。フレーム描画とカーソル移動が 1 回の
  書き込みにまとまるので IME オーバーレイと競合しにくい。
- **x/vt は Cell に `Width` を持ち**、grapheme 単位で幅計算する。全角ズレの構造的原因が消える。

## プロトタイプの構造 (`cmd/vtspike/main.go`)

- `startChild`: `claude` を PTY で起動し、`vt.NewEmulator(cols, rows)` に接続。
  emulator→pty を `io.Copy` で繋ぎ、DSR/DA 応答・キー列・ペーストブラケットを素通し。
- `pump`: PTY を読んで emulator に書く。**16ms に 1 フレームへ間引き**して
  チャッティな子でもレンダラを溢れさせない (前回は 150ms タイマー)。
- カーソルは `SetCallbacks` の `CursorVisibility` / `CursorStyle` で追従。
- `View`: emulator を行ごとに幅パディングして描画し、フォーカス時のみ
  `View.Cursor` に実カーソルを設定。左に統計ペイン、右に子端末。
- キー: `Ctrl+]` フォーカス切替 / `Ctrl+Q` 終了。それ以外は `emu.SendKey` で子へ。
  ペースト・マウスホイールも子へ透過。

## 自動検証の結果 (擬似端末ハーネス)

Python `pty.fork` で vtspike を起動し出力バイト列を検査。

| 確認項目 | 結果 |
|---|---|
| claude が右ペインに起動し信頼確認ダイアログ・`❯` 選択 UI が本物のまま描画 | OK |
| 全角文字列 `こんにちは世界` が列ズレなくセルに乗る | OK |
| カーソル位置報告 (CSI 6n / DSR)・代替スクリーン (1049) | OK |
| Primary Device Attributes (DA1) 応答 | OK |
| 同期出力モード等の問い合わせ (DECRQM `?2026$p`) に応答 | OK |
| 実カーソル座標の同期 (前回欠落) が配線済み | OK |
| 依存追加後も `go build ./...` / `go vet` / `go test ./internal/...` green | OK |

claude は起動時に `?2026$p` `?2027$p`（同期出力/DECRQM）と `>1u`（kitty keyboard）を
投げるが、x/vt が DECRQM に応答し、応答しない拡張があっても claude はダイアログまで
描画を進めたため **ハード停止は無し**。

## 自動検証で確認**できない**こと — IME (要 人手)

擬似端末では OS の日本語変換 (macOS/Windows/fcitx の pre-edit オーバーレイ) を
再現できない。**これが前回の実装を葬った本丸**であり、実端末で人が変換入力して
確かめるしかない。生バイトで `テスト入力` を流す試みは、claude が信頼ダイアログ上に
いたため入力欄に届かず、IME 判定としては不成立。

### 手元での確認手順

```sh
go build ./cmd/vtspike && ./vtspike
```

右ペイン (起動時フォーカス = RIGHT) で claude の入力欄に日本語を変換入力し、次を確認:

1. 変換中 (未確定) の下線付き文字が正しいカーソル位置に出るか
2. 変換中に幽霊スペース/チラつきが出ないか
3. 確定後の文字が列ズレせず入るか

Terminal.app に加え Ghostty / iTerm2 でも試すと確実。`Ctrl+]` フォーカス切替 / `Ctrl+Q` 終了。

## 判定と次段

- **自動で確認できる範囲は全て green。** 前回を葬った 2 壁のうち、全角幅は解決、
  実カーソル同期も配線済みで、理屈上 IME も解ける状態。
- **残るゲートは実端末の IME 目視のみ。** ここが通れば当初計画通り進める:
  1. Bubble Tea v2 移行 PR (挙動据え置き、View 戻り値型とキー/マウス msg 型の差し替え)
  2. 右ペイン改修 (emulator をサーバー側へ、TUI はセル差分を受けて描画)
- IME でまだ乱れる場合は、改修着手前に描画抑制方法を詰め直す。

## リスク・留意

- **最重量は tui.go (~3400 行) の v1→v2 移行。** View 戻り値型、キー/マウス msg 型が変わる。
- **x/vt は pre-v1 でタグ無し。** コミット固定で追従する運用になる。
- **IME は理屈上解決だが実端末で打つまで断言不可。**
- **ミラーできないもの**: 他端末/tmux で動く既存セッションの画面 (他人の PTY は覗けない)。
  それらは要約表示のまま Enter で tmux 切替を残す。停止中セッションは `--resume` で
  ccdash の PTY 側に引き取れば以後は仮想ターミナル化できる。
- 右ペインは「ccdash 配下で動くセッション = 本物の画面 / それ以外 = 要約表示」の二層になる。
