# hishaku リポジトリ規約

hishaku は agent のセッションを 4 桁の番号で預かり、動かす機械へ注ぐ。`worker/` が正本を持つ Manager（Cloudflare Worker、pnpm workspace）、`cmd/hs` と `internal/` が貸し出しと記録の出し入れを行う Go の CLI `hs`、`api/openapi.yaml` が Manager の API の定義（OpenAPI 3.1）。設計は issue #1、層と API の概要は README にある。Access のアプリと service token は `ochanuco/chabatake` が持ち、R2 バケット・独自ドメインはこの repo の `worker/wrangler.jsonc` が持つ。

## コマンド

- `pnpm install`（`worker` の postinstall で `wrangler types`）
- `pnpm run typecheck && pnpm run lint && pnpm test && pnpm run build`: PR 前に必ず通す
- `pnpm format`: Biome で整形（lint も Biome）

## 規約

- public repo なので、メールアドレス・Discord の ID・アカウント ID・内部のホスト名などの実値をコード・テスト・docs に書かない。テストは example.com と架空の ID を使う
- Manager の API を変えるときは `api/openapi.yaml` を同じ PR で直す。CLI はこの定義を見て作る
- Valibot は境界（リクエストの入力）でのみ使う。内部の型は TypeScript の型で表す
- 想定内の失敗は例外を投げず `{ ok: false, error }` で返す
- Worker のデプロイは main への push で `deploy` workflow が行う。`wrangler deploy` の手打ちはしない
- コメントは日本語で、コードから読み取れない理由だけを書く
