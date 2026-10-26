#!/bin/sh
# Production entrypoint. Postgres is used as-is. SQLite in production is
# restored from Litestream (if a replica exists) and then replicated while
# OpenGate runs. Secrets are environment variables, never this script.
set -eu

if [ "$#" -eq 0 ]; then
  set -- serve -config /etc/opengate/cloud.yaml
fi

db="${OPENGATE_DATABASE:-/data/opengate.db}"
case "$db" in
  postgres://*|postgresql://*)
    exec /usr/local/bin/opengate "$@"
    ;;
esac

prod=0
case "${OPENGATE_ENV:-}" in
  production|prod) prod=1 ;;
esac
if [ "$prod" -eq 1 ] && [ -z "${LITESTREAM_BUCKET:-}" ] && [ "${OPENGATE_SQLITE_NO_REPLICA:-}" != "1" ]; then
  echo "production SQLite requires LITESTREAM_BUCKET, or set OPENGATE_DATABASE to a postgres:// URL" >&2
  exit 1
fi

if [ "$1" = "serve" ] && [ -n "${LITESTREAM_BUCKET:-}" ]; then
  export AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-${LITESTREAM_ACCESS_KEY_ID:-}}"
  export AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-${LITESTREAM_SECRET_ACCESS_KEY:-}}"
  qs=""
  if [ -n "${LITESTREAM_ENDPOINT:-}" ]; then
    qs="endpoint=${LITESTREAM_ENDPOINT}"
  fi
  if [ -n "${LITESTREAM_REGION:-}" ]; then
    if [ -n "$qs" ]; then qs="${qs}&"; fi
    qs="${qs}region=${LITESTREAM_REGION}"
  fi
  if [ "${LITESTREAM_FORCE_PATH_STYLE:-}" = "1" ] || [ "${LITESTREAM_FORCE_PATH_STYLE:-}" = "true" ]; then
    if [ -n "$qs" ]; then qs="${qs}&"; fi
    qs="${qs}force-path-style=true"
  fi
  export LITESTREAM_REPLICA_URL="s3://${LITESTREAM_BUCKET}/opengate"
  if [ -n "$qs" ]; then
    export LITESTREAM_REPLICA_URL="${LITESTREAM_REPLICA_URL}?${qs}"
  fi
  mkdir -p "$(dirname "$db")"
  litestream restore -if-replica-exists -if-db-not-exists -config /etc/litestream.yml "$db"
  cmd="/usr/local/bin/opengate"
  for a in "$@"; do
    cmd="$cmd $a"
  done
  exec litestream replicate -config /etc/litestream.yml -exec "$cmd"
fi

exec /usr/local/bin/opengate "$@"
