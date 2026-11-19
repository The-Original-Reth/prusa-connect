#!/bin/sh
# Bring this Prusa Connect up to date in one go: the newest code from GitHub (this fork), then the server rebuilt
# and started on the host's network (compose.host.yml: without it a camera's live view never starts).
# Printers reconnect by themselves once it's back; a print under way carries on. Run it from anywhere:
#   sudo sh update.sh
set -e
cd "$(dirname "$0")"
COMPOSE="docker compose -f docker-compose.local.yml -f compose.host.yml"
git pull
$COMPOSE up -d --build prusa-connect
printf 'Waiting for Prusa Connect to answer'
for _ in $(seq 1 40); do
  if [ "$(docker inspect -f '{{.State.Health.Status}}' prusa_connect 2>/dev/null)" = healthy ]; then
    echo " - it's up."
    $COMPOSE ps prusa-connect
    exit 0
  fi
  printf '.'
  sleep 3
done
echo
echo "Prusa Connect didn't say it's healthy within 2 minutes. Its log: docker logs --tail 50 prusa_connect" >&2
exit 1
