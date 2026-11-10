# hishaku

Agent session routing: hold short-lived agent sessions and pour them to whichever machine runs them.

agent（Claude Code、Codex）のセッションを、投稿ごとの 4 桁の番号で預かり、動かす機械（Commander）へ注ぐ。
ある機械で始めた会話を、別の機械でそのまま `claude -r` などで続けられるようにする。SSH は使わない。

## 層

| 層         | 持ち主                                  | 知っていること                                                                |
| ---------- | --------------------------------------- | ----------------------------------------------------------------------------- |
| 起動       | Orca、Working の受付                    | worktree、端末、そこで何を走らせるか                                          |
| セッション | `hs` CLI（Go、`cmd/hs` と `internal/`） | 貸し出し、正本の出し入れ、agent ごとの記録の場所と起こし方、非対話の 1 ターン |
| 正本       | Manager（Cloudflare Worker、`worker/`） | 番号ごとの記録と agent の種類。記録の中身は読まない                           |

```
worker/      TypeScript（pnpm、wrangler、vitest）: Manager
cmd/hs/      Go: CLI
internal/    Go: agent のアダプタ、Manager のクライアント
api/         Manager の API の定義（OpenAPI 3.1、api/openapi.yaml）
```

## Manager

- 番号は 1000〜9999 のランダムな数字で、生きている番号とは重ならない。期限で消えた番号はまた使う。
- Discord の投稿 ID は `post_id` として meta に持つ。同じ `post_id` で払い出すと、生きている番号を返す。
- 期限は最後の更新から 30 日。期限が来たら Durable Object の alarm が meta と R2 の記録を消す。
- 番号・meta・貸し出し・世代は Durable Object `SessionRegistry`（1 つだけ、SQLite の表 1 枚）が持つ。貸し出しの排他はここが要求を 1 つずつ受けることで守る。
- 記録は R2 の `sessions/{number}/transcript.jsonl`、取られた側の枝は `sessions/{number}/forks/<UTC の yyyymmdd-hhmmss>.jsonl`。
- 認証は Cloudflare Access の service token（`CF-Access-Client-Id` / `CF-Access-Client-Secret`）。Worker は Access を通ったものだけを受ける前提で、自前の認証は持たない。

### API（v1）

詳しくは [`api/openapi.yaml`](api/openapi.yaml)。

| メソッドと道                                               | 中身                                                                                                                                                                                                     |
| ---------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `POST /v1/sessions`                                        | `{ post_id, agent, title, repo?, branch? }` で番号を払い出す。同じ `post_id` の生きている番号があればそれを返す                                                                                          |
| `GET /v1/sessions?post_id=<id>`                            | `post_id` から引く                                                                                                                                                                                       |
| `GET /v1/sessions/{number}`                                | meta を返す。`pushed_at` は最後に記録を PUT した時刻（forks では変わらない）、`release_requested_by` は返してほしいと頼んだ Commander                                                                    |
| `PATCH /v1/sessions/{number}`                              | `title`・`repo`・`branch`・`session_id` を更新する                                                                                                                                                       |
| `GET /v1/sessions/{number}/transcript`                     | 正本の記録（jsonl）。無ければ 404。`X-Hishaku-Generation` に今の世代                                                                                                                                     |
| `PUT /v1/sessions/{number}/transcript?holder=&generation=` | 記録を置き換える。今の貸し出しと合わなければ 409                                                                                                                                                         |
| `POST /v1/sessions/{number}/forks`                         | 取られた側の記録を枝として残し、`{ key }` を返す                                                                                                                                                         |
| `POST /v1/sessions/{number}/lease`                         | `{ holder, mode: "acquire" \| "takeover", ttl_seconds }`。acquire は持ち主がいれば 409 で `{ holder, lease_expires_at }`。通れば世代を 1 上げて `{ generation, lease_expires_at }`                       |
| `POST /v1/sessions/{number}/lease/renew`                   | `{ holder, generation }` で期限を延ばす。合わなければ 409。応答の `release_requested_by` に返してほしいと頼んだ Commander                                                                                |
| `POST /v1/sessions/{number}/lease/request`                 | `{ holder }` で今の持ち主に返してほしいと頼み、`{ holder, generation, lease_expires_at, release_requested_by }` を返す。誰も持っていなければ 409 `lease_free`、頼んだ側が持ち主なら 409 `lease_mismatch` |
| `DELETE /v1/sessions/{number}/lease`                       | `{ holder, generation }` で返す。合わなければ 409                                                                                                                                                        |

`GET /health` は生存確認用。

例（ホスト名は架空）:

```sh
curl -sS https://hishaku.example.com/v1/sessions \
  -H "CF-Access-Client-Id: $HISHAKU_ACCESS_CLIENT_ID" \
  -H "CF-Access-Client-Secret: $HISHAKU_ACCESS_CLIENT_SECRET" \
  -H 'Content-Type: application/json' \
  -d '{"post_id":"123","agent":"claude","title":"例"}'
```

### MCP

`POST /mcp` は、預かっているセッションを読むだけの MCP サーバー（Streamable HTTP、状態を持たない）。
Mac 以外の端末や別の Claude Code から、番号の会話を新しいセッションに文脈として読ませるのに使う。`claude -r` で再開するのとは別の使い方。
API と同じ Access の後ろにあり、自前の認証は持たない。

| 道具            | 引数                                                  | 返すもの                                                                                                                       |
| --------------- | ----------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| `list_sessions` | `limit`（既定 20、最大 100）、`query`（件名に含む語） | 生きている番号を最後に更新された順に。各行は `number`・`title`・`agent`・`repo`・`branch`・`holder`・`pushed_at`・`updated_at` |
| `get_session`   | `number`、`max_chars`（既定 20000）                   | meta と、正本の記録を要点に絞った会話。記録が無ければ meta だけ                                                                |

`get_session` の会話は、Claude の jsonl と Codex の rollout を meta の `agent` で読み分け、次だけを残す。

- 人の発言と agent の発言の本文（Claude Code や Codex が差し込む指示・環境の情報は除く）
- ツール呼び出しの名前と、入力の短い要約
- ツールのエラーの 1 行（ツールの出力そのものは省く）

`max_chars` を超える分は古い側から削り、新しい側を残して、削ったことを会話の先頭に書く。

返す前に、よく知られた形の token（`ghp_` などの GitHub の token、`github_pat_`、1Password の `ops_`、`sk-`、Slack の `xox?-`、AWS のアクセスキー、Google の API キー、JWT、`-----BEGIN ... PRIVATE KEY-----` の塊、`CF-Access-Client-Secret:` や `Authorization:` の値）を `[redacted]` に伏せる。
形で拾うだけなので、パスワードや独自の形の鍵など、ここに無い秘密は伏せずに返る。記録に秘密を残さないことが先で、伏せ字は保険にすぎない。

Claude Code からつなぐ（Access の service token は `hs` と同じものでよい）:

```sh
claude mcp add -s user -t http hishaku https://hishaku.chanu.co/mcp \
  -H "CF-Access-Client-Id: $HISHAKU_ACCESS_CLIENT_ID" \
  -H "CF-Access-Client-Secret: $HISHAKU_ACCESS_CLIENT_SECRET"
```

`-H` は後ろの引数を続けて取るので、名前と URL の後に置く。値は追加したときに展開され、`~/.claude.json` に平文で残る。

## 開発

Node 24 と pnpm（`mise install` で入る）。

```sh
pnpm install                  # worker の postinstall で wrangler types
pnpm run typecheck
pnpm run lint                 # Biome
pnpm test                     # vitest（@cloudflare/vitest-plugin で workerd の中で走る）
pnpm run build                # wrangler deploy --dry-run
pnpm format
pnpm --filter @hishaku/worker dev   # wrangler dev
```

## デプロイ

main への push で `.github/workflows/deploy.yml` が `worker/scripts/deploy.sh production` を走らせ、`wrangler deploy` する。
承認は前回の記録が無いとき・ロールバックのとき・`worker/wrangler.jsonc` が変わったときだけ Environment `production-approval` で止まる。
`CLOUDFLARE_API_TOKEN` は Environment `production` の `OP_SA_DEPLOY` から `op run` で渡る（`.github/deploy/production.env.op`）。

独自ドメインと R2 のバインディングは `worker/wrangler.jsonc` が持ち、独自ドメインと DNS は `wrangler deploy` が作る。
Access のアプリと service token は `ochanuco/chabatake` が持つ。

### 初回だけ

R2 バケットは `wrangler deploy` では作られないので、最初のデプロイの前に作る。

```sh
cd worker
pnpm exec wrangler login        # または CLOUDFLARE_API_TOKEN を渡す
pnpm exec wrangler r2 bucket create hishaku-transcripts
```
