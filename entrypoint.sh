#!/bin/sh
set -e

DB=/pb/pb_data/data.db
PB_CMD="/pb/pocketbase serve --http=0.0.0.0:8090"

# No S3 target configured? Run PocketBase directly, no replication.
# (Local dev convenience — there is NO durability in this mode.)
if [ -z "$LITESTREAM_BUCKET" ]; then
  echo "[entrypoint] LITESTREAM_BUCKET unset — running PocketBase WITHOUT Litestream."
  exec $PB_CMD
fi

# 1. Restore-on-boot: if there's no local DB but a replica exists, pull it down.
#    This MUST happen before PocketBase opens the file — that's why Litestream
#    lives in this container and not a separate sidecar container.
if [ ! -f "$DB" ]; then
  echo "[entrypoint] no local DB found — attempting restore from replica..."
  litestream restore -if-replica-exists "$DB"
else
  echo "[entrypoint] local DB present — skipping restore."
fi

# 2. Hand off to Litestream as the supervisor: it streams the WAL to S3 AND
#    runs PocketBase as a child process. If PB exits, Litestream does a final
#    sync and exits too.
echo "[entrypoint] starting Litestream replication + PocketBase..."
exec litestream replicate -exec "$PB_CMD"
