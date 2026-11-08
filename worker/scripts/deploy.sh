#!/usr/bin/env bash
# 使い方: worker/scripts/deploy.sh production
# CLOUDFLARE_API_TOKEN は呼び出し側（deploy workflow の op run）が環境変数で渡す。Worker に secret は無い。
set -euo pipefail

if [ "${1:-}" != "production" ]; then
  echo "usage: worker/scripts/deploy.sh production" >&2
  exit 2
fi

cd "$(dirname "$0")/.."

if [ -z "${CLOUDFLARE_API_TOKEN:-}" ]; then
  echo "error: 未設定: CLOUDFLARE_API_TOKEN（.github/deploy/production.env.op の参照先を確かめる）" >&2
  exit 1
fi

pnpm exec wrangler deploy
