# hs

agent（Claude Code、Codex）のセッションを 4 桁の番号で Manager に預け、別の機械で続けるための CLI。

## 入れ方

mise の `github:` バックエンドで GitHub Releases のバイナリを入れる。darwin/arm64、linux/amd64、linux/arm64 がある。

```sh
mise use -g github:ochanuco/hishaku
```

手元で作るなら `go install github.com/ochanuco/hishaku/cmd/hs@latest`。

## 環境変数

| 名前 | 中身 |
|---|---|
| `HISHAKU_URL` | Manager の URL。必須 |
| `HISHAKU_ACCESS_CLIENT_ID` | Cloudflare Access の service token の Client ID。`CF-Access-Client-Id` ヘッダーで送る |
| `HISHAKU_ACCESS_CLIENT_SECRET` | 同じく Client Secret。`CF-Access-Client-Secret` ヘッダーで送る |
| `HISHAKU_HOLDER` | この機械の名前（Commander の ID）。既定はホスト名の最初のラベルを小文字にしたもの |
| `ORCA_CLI_COMMAND` | `hs open` が使う orca の実行ファイル。既定は `orca` |
| `CLAUDE_CONFIG_DIR` | Claude Code の記録の置き場所。既定は `~/.claude` |
| `CODEX_HOME` | Codex の記録の置き場所。既定は `~/.codex` |

## コマンド

### `hs resume [--takeover] <number>`

番号の貸し出しを受け、正本の記録を agent の置き場所に置いて agent を起こす。agent の画面と入力はそのまま使える。

- 他の機械が借りていれば、持ち主と期限を出して終わる。`--takeover` を付けると奪う。
- 正本に記録が無ければ新しいセッションを始め、その session_id を meta に残す。
- 動いている間は 30 秒ごとに、記録が変わっていれば預け、貸し出しを延ばす。
- agent が終わったら最後の記録を預けて貸し出しを返す。終了コードは agent のものをそのまま返す。
- 途中で他の機械に取られたら「取られた」と出し、以後の記録は正本を上書きせず forks に枝として預ける。
- SIGTERM は agent に渡し、agent が終わってから最後の処理をする。端末の Ctrl-C は agent が直接受け取る。

agent ごとの記録の置き場所と起こし方:

| agent | 記録 | 再開 | 新規 |
|---|---|---|---|
| claude | `$CLAUDE_CONFIG_DIR/projects/<cwd の英数字以外を - にした名前>/<session_id>.jsonl` | `claude -r <session_id>` | `claude --session-id <uuid>` |
| codex | `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<時刻>-<session_id>.jsonl` | `codex resume <session_id>` | `codex`（起こした後に記録から session_id を拾う） |

### `hs open [-y] [org/repo] <number>`

番号の件名・repo・ブランチ・agent を出して確かめ（`-y` で省く）、Orca に worktree `hs-<number>` を立てて `hs resume <number>` を走らせる端末を開く。

- repo を省くと meta の repo を使う。Orca に登録された repo から remote（無ければ path の末尾）で探す。
- worktree `hs-<number>` が既にあれば作らずに使う。無ければ meta のブランチで作る（agent は起こさない）。
