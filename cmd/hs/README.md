# hs

agent（Claude Code、Codex）のセッションを 4 桁の番号で Manager に預け、別の機械で続けるための CLI。

## 入れ方

mise の `github:` バックエンドで GitHub Releases のバイナリを入れる。darwin/arm64、linux/amd64、linux/arm64 がある。

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

## コマンド

### `hs resume [--takeover] <number> [-- <agent の引数>]`

番号の貸し出しを受け、正本の記録を agent の置き場所に置いて agent を起こす。agent の画面と入力はそのまま使える。

- `--` の後ろは agent にそのまま渡す（`--permission-mode auto`、`--model`、Codex の `-a never -s danger-full-access` など）。Claude では起こすコマンドの後ろに、Codex では `resume` の前に置く。

- 他の機械が借りていれば、持ち主と期限を出して終わる。`--takeover` を付けると奪う。
- 正本に記録が無ければ新しいセッションを始め、その session_id を meta に残す。
- 動いている間は 30 秒ごとに、記録が変わっていれば預け、貸し出しを延ばす。
- agent が終わったら最後の記録を預けて貸し出しを返す。終了コードは agent のものをそのまま返す。
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
- meta にも引数にも repo が無ければ worktree を作らず、その端末のカレントで `hs resume <number>` と同じように前の会話を開く。
- worktree `hs-<number>` が既にあれば作らずに使う。無ければ meta のブランチで作る（agent は起こさない）。

### agent を呼ぶ側が起こすとき

端末の先頭を agent にしたい呼び手（mizuya の Working など）は、`hs resume` の代わりに次の 3 つを使い、agent のコマンドは自分で組み立てる。`--holder` の既定は `HISHAKU_HOLDER`、次にホスト名から作る値。

#### `hs prepare <number> [--holder <id>] [--cwd <dir>] [--takeover] [--json]`

貸し出しを受け、正本の記録を `--cwd`（既定はカレント）を基準にした agent の置き場所に置く。`--json` で標準出力に 1 行の JSON を出す。

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

#### `hs renew <number> --generation N [--holder <id>]`

貸し出しを延ばす。期限は 2 分なので、それより短い間隔で呼ぶ。

`hs push` と `hs renew` の終了コード:

| 終了コード | 意味                                                                      |
| ---------- | ------------------------------------------------------------------------- |
| 0          | 成功                                                                      |
| 3          | 貸し出しは他に取られていた（409）。push は記録を forks に預けてから終わる |
| 1          | そのほか                                                                  |
