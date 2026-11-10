# hs

agent（Claude Code、Codex）のセッションを 4 桁の番号で Manager に預け、別の機械で続けるための CLI。

## 入れ方

mise の `github:` バックエンドで GitHub Releases のバイナリを入れる。darwin/arm64、linux/amd64、linux/arm64 がある。

リリースは `cmd/`・`internal/`・`go.mod`・`go.sum`・`.goreleaser.yaml` の変更が main に入ると、`go-release` の workflow がパッチ版を上げて出す。minor や major を上げるときは、その workflow を手動で実行して `bump` を選ぶ。

```sh
mise use -g github:ochanuco/hishaku
```

手元で作るなら `go install github.com/ochanuco/hishaku/cmd/hs@latest`。

## 環境変数

| 名前                           | 中身                                                                                  |
| ------------------------------ | ------------------------------------------------------------------------------------- |
| `HISHAKU_URL`                  | Manager の URL。必須                                                                  |
| `HISHAKU_ACCESS_CLIENT_ID`     | Cloudflare Access の service token の Client ID。`CF-Access-Client-Id` ヘッダーで送る |
| `HISHAKU_ACCESS_CLIENT_SECRET` | 同じく Client Secret。`CF-Access-Client-Secret` ヘッダーで送る                        |
| `HISHAKU_HOLDER`               | この機械の名前（Commander の ID）。既定はホスト名の最初のラベルを小文字にしたもの     |
| `ORCA_CLI_COMMAND`             | `hs open` が使う orca の実行ファイル。既定は `orca`                                   |
| `CLAUDE_CONFIG_DIR`            | Claude Code の記録の置き場所。既定は `~/.claude`                                      |
| `CODEX_HOME`                   | Codex の記録の置き場所。既定は `~/.codex`                                             |
| `HISHAKU_REPORT_URL`           | `hs ask` と `hs status` の送り先。席を動かす側が agent に渡す                         |
| `HISHAKU_REPORT_TOKEN`         | 送り先に `Authorization: Bearer` で付ける token                                       |
| `HISHAKU_NUMBER`               | `hs ask` と `hs status` の番号。`hs turn` が agent に渡す                             |

## コマンド

### `hs resume [--takeover] [--wait|--no-wait] <number> [-- <agent の引数>]`

番号の貸し出しを受け、正本の記録を agent の置き場所に置いて agent を起こす。agent の画面と入力はそのまま使える。

- `--` の後ろは agent にそのまま渡す（`--permission-mode auto`、`--model`、Codex の `-a never -s danger-full-access` など）。Claude では起こすコマンドの後ろに、Codex では `resume` の前に置く。

- 他の機械が借りていれば、持ち主に返してほしいと頼み、5 秒おきに借りられるかを試す。借りられたら続ける。待つ間は「<番号> は <holder> が使っています。返してもらうのを待っています（Ctrl-C でやめる）」を 1 度出す。
- 待つのは標準入力が端末のときの既定。`--no-wait`（端末でなければ既定）では待たずに持ち主と期限を出して 2 で終わる。`--takeover` を付けると待たずに奪う。
- 正本に記録が無ければ新しいセッションを始め、その session_id を meta に残す。
- 動いている間は 30 秒ごとに、記録が変わっていれば預け、貸し出しを延ばす。
- agent が終わったら最後の記録を預けて貸し出しを返す。終了コードは agent のものをそのまま返す。
- 動いている間に他の Commander から返してほしいと頼まれたら「<holder> が <番号> を返してほしいと言っています。終わったら agent を閉じてください」を 1 度出す。agent は止めない。閉じれば記録を預けて返すので、待っている側が借りる。
- 途中で他の機械に取られたら「取られた」と出し、以後の記録は正本を上書きせず forks に枝として預ける。
- SIGTERM は agent に渡し、agent が終わってから最後の処理をする。端末の Ctrl-C は agent が直接受け取る。

agent ごとの記録の置き場所と起こし方:

| agent  | 記録                                                                               | 再開                        | 新規                                              |
| ------ | ---------------------------------------------------------------------------------- | --------------------------- | ------------------------------------------------- |
| claude | `$CLAUDE_CONFIG_DIR/projects/<cwd の英数字以外を - にした名前>/<session_id>.jsonl` | `claude -r <session_id>`    | `claude --session-id <uuid>`                      |
| codex  | `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<時刻>-<session_id>.jsonl`                | `codex resume <session_id>` | `codex`（起こした後に記録から session_id を拾う） |

### `hs open [-y] [org/repo] <number>`

番号の件名・repo・ブランチ・agent を出して確かめ（`-y` で省く）、Orca に worktree `hs-<number>` を立てて `hs resume <number>` を走らせる端末を開く。

- repo を省くと meta の repo を使う。Orca に登録された repo から remote（無ければ path の末尾）で探す。
- 端末の `hs resume` は既定のまま起こすので、他が借りていれば返してもらうのを待つ。
- meta にも引数にも repo が無ければ worktree を作らず、その端末のカレントで `hs resume <number>` と同じように前の会話を開く。
- worktree `hs-<number>` が既にあれば作らずに使う。無ければ作る（agent は起こさない）。
- meta にブランチがあれば、Orca の repo の path で `git fetch origin <branch>` してから、`origin/<branch>` を起点に同じ名前のブランチで worktree を作る。fetch に失敗したら理由を出して止まり、worktree は作らない。
- meta にブランチが無ければ、Orca の repo の既定の base から作る。

### agent を呼ぶ側が起こすとき

端末の先頭を agent にしたい呼び手（mizuya の Working など）は、`hs resume` の代わりに次のコマンドを使い、agent のコマンドは自分で組み立てる。`--holder` の既定は `HISHAKU_HOLDER`、次にホスト名から作る値。

#### `hs prepare <number> [--holder <id>] [--cwd <dir>] [--takeover] [--wait] [--json]`

貸し出しを受け、正本の記録を `--cwd`（既定はカレント）を基準にした agent の置き場所に置く。`--json` で標準出力に 1 行の JSON を出す。`--wait` を付けると、他が借りていれば `hs resume` と同じように返してほしいと頼んで待つ（既定は待たずに 2 で終わる）。

```text
{"number":1234,"agent":"claude","session_id":"<id>","resume":true,"generation":3,"lease_expires_at":"2026-01-01T00:02:00Z"}
```

| 状況                 | `resume` | `session_id`               | 呼ぶ側が起こすコマンド                 |
| -------------------- | -------- | -------------------------- | -------------------------------------- |
| 正本に記録がある     | `true`   | meta のもの                | `claude -r <id>` / `codex resume <id>` |
| 記録が無い（claude） | `false`  | 新しい uuid（meta に残す） | `claude --session-id <id>`             |
| 記録が無い（codex）  | `false`  | 空                         | `codex`（ID は `hs push` が拾う）      |

| 終了コード | 意味                                                                |
| ---------- | ------------------------------------------------------------------- |
| 0          | 成功                                                                |
| 2          | 他が借りている。`--json` なら `holder` と `lease_expires_at` を出す |
| 1          | そのほか                                                            |

#### `hs push <number> --generation N [--holder <id>] [--cwd <dir>] [--release]`

agent の記録を見つけて正本に預ける。`--release` で預けた後に貸し出しを返す。

- Codex で session_id がまだ無ければ、`hs prepare` の後に `--cwd` で書かれた最新の rollout から拾って meta に残す。そのため prepare と push は同じ機械で呼ぶ。
- 記録が空か、前に預けたときから変わっていなければ PUT しない。

#### `hs renew <number> --generation N [--holder <id>] [--json]`

貸し出しを延ばす。期限は 2 分なので、それより短い間隔で呼ぶ。`--json` で標準出力に 1 行の JSON を出す。`release_requested_by` は返してほしいと頼んだ Commander（誰も頼んでいなければ `null`）で、入っていれば区切りで agent を閉じ、`hs push --release` で返す。

```text
{"generation":3,"lease_expires_at":"2026-01-01T00:02:00Z","release_requested_by":"mac-a"}
```

`hs push` と `hs renew` の終了コード:

| 終了コード | 意味                                                                      |
| ---------- | ------------------------------------------------------------------------- |
| 0          | 成功                                                                      |
| 3          | 貸し出しは他に取られていた（409）。push は記録を forks に預けてから終わる |
| 1          | そのほか                                                                  |

#### `hs request <number> [--holder <id>] [--json]`

今の持ち主に返してほしいと頼む。持ち主は次の renew でそれを知る。`--json` で今の持ち主・世代・期限と頼んだ Commander を 1 行の JSON で出す。

```text
{"holder":"docker01","generation":3,"lease_expires_at":"2026-01-01T00:02:00Z","release_requested_by":"mac-a"}
```

| 終了コード | 意味                                 |
| ---------- | ------------------------------------ |
| 0          | 頼んだ                               |
| 5          | 誰も借りていない                     |
| 1          | そのほか（自分が持ち主のときを含む） |

### agent を非対話で 1 ターンずつ走らせるとき

端末を持たない呼び手（docker01 の Working など）は、`hs prepare` で借りて `hs renew` で延ばしながら、指示が来るたびに `hs turn` で 1 ターンずつ走らせる。終わったら `hs push --release` で返す。

#### `hs turn <number> --generation N [--holder <id>] [--cwd <dir>] [--prompt-file <f>] [-- <agent の引数>]`

指示（`--prompt-file`、無ければ標準入力）を agent の標準入力に渡して非対話で 1 ターン走らせ、終わったら記録を預ける。貸し出しは返さず、延ばしもしない。結果は標準出力に 1 行の JSON で出す。agent の標準エラーは hs の標準エラーにそのまま出す。

```text
{"number":1234,"agent":"claude","session_id":"<id>","exit_code":0,"text":"<最後のメッセージ>","failed":false,"stolen":false}
```

- `--cwd` で agent を起こす。agent の環境には `HISHAKU_NUMBER` を足し、`HISHAKU_ACCESS_CLIENT_ID` と `HISHAKU_ACCESS_CLIENT_SECRET` を外す。
- 手元に session_id の記録があれば再開し、無ければ新しいセッションを始める。Codex の session_id は出力の `thread.started` から拾って meta に残す。
- 失敗したターン（`failed: true`）でも記録は預ける。理由は `reason` に入る。記録を預けられなければ `push_error` に理由を入れる。
- SIGINT と SIGTERM は agent に渡し、agent が終わってから記録を預ける。

#### `hs turn --agent <claude|codex> [--session <id> [--resume] | --continue] [--cwd <dir>] [--prompt-file <f>] [-- <agent の引数>]`

番号を持たない席（Manager に届かないときを含む）で、Manager を使わずに 1 ターン走らせる。結果の JSON と終了コードは番号のときと同じで、`number` を出さない。

- `--session <id>` だけなら、その ID で新しいセッションを始める（Codex は ID を指定できないので無視する）。`--resume` を付けるとその会話を続ける。
- `--continue` は作業場所の最新の会話を続ける。
- どれも無ければ新しいセッションを始める。始めたセッションの ID は結果の `session_id` に入る。

| agent  | 再開                                                  | 最新を続ける（`--continue`）                       | 新規                                                      |
| ------ | ----------------------------------------------------- | -------------------------------------------------- | --------------------------------------------------------- |
| claude | `claude -p --output-format json <引数> --resume <id>` | `claude -p --output-format json <引数> --continue` | `claude -p --output-format json <引数> --session-id <id>` |
| codex  | `codex exec <引数> --json -o <f> resume <id> -`       | `codex exec <引数> --json -o <f> resume --last -`  | `codex exec <引数> --json -o <f> -`                       |

| 終了コード | 意味                                                  |
| ---------- | ----------------------------------------------------- |
| 0          | ターンが成功した                                      |
| 4          | ターンが失敗した。JSON は出す                         |
| 3          | 貸し出しは他に取られていた。記録は forks に預けてある |
| 1          | agent を起こす前の失敗（JSON は出さない）             |

#### `hs ask --body <本文|-> [--subject <件名>] [--number <number>]`

#### `hs status --body <本文|-> [--subject <件名>] [--number <number>]`

`hs turn` で走っている agent が、持ち主への質問（ask）とターンの途中の知らせ（status）を席を動かす側に送る。`--body -` で本文を標準入力から読む。番号の既定は `HISHAKU_NUMBER` で、無ければ番号を付けずに送る。番号の無い席は、送り先が `HISHAKU_REPORT_URL` を席ごとに分けて見分ける。待たずに終わるので、ask の後は agent がターンを終え、答えは次のターンの指示として受け取る。

`HISHAKU_REPORT_URL` に次の JSON を POST する。2xx 以外は 1 で終わる。

```text
{"number":1234,"type":"ask","subject":"<件名>","body":"<本文>"}
```
