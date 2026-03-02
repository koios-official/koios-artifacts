#!/bin/bash
DB_NAME=cexplorer

tip=$(psql ${DB_NAME} -qbt -c "select extract(epoch from time)::integer from block order by id desc limit 1;" | xargs)

if [[ $(( $(date +%s) - tip )) -gt 300 ]]; then
  echo "$(date +%F_%H:%M:%S) Skipping as database has not received a new block in past 300 seconds!" && exit 1
fi

echo "$(date +%F_%H:%M:%S) Running pool history cache update..."
if ! psql ${DB_NAME} -v ON_ERROR_STOP=1 -qbt -c "CALL GREST.pool_history_cache_update();" 1>/dev/null; then
  echo "$(date +%F_%H:%M:%S) Error: pool history cache update failed!" >&2
  exit 1
fi
echo "$(date +%F_%H:%M:%S) Job done!"
