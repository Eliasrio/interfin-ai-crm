#!/usr/bin/env bash
# M7: генерация RSA-пары для JWT RS256 (SRS §5.1) в ./secrets/.
# Ключи НИКОГДА не коммитятся (CLAUDE.md §4.9): secrets/ и *.pem в .gitignore.
# docker compose монтирует их как Docker secrets jwt_private / jwt_public.
set -euo pipefail

dir="$(cd "$(dirname "$0")/.." && pwd)/secrets"
priv="$dir/jwt_private.pem"
pub="$dir/jwt_public.pem"

if [[ -f "$priv" || -f "$pub" ]]; then
  echo "secrets/jwt_*.pem уже существуют — не перезаписываю." >&2
  echo "Смена ключей инвалидирует все выданные access-токены; удали файлы вручную." >&2
  exit 1
fi

mkdir -p "$dir"
umask 077
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$priv"
openssl pkey -in "$priv" -pubout -out "$pub"
echo "Готово: $priv, $pub"
