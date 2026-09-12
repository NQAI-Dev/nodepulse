#!/bin/sh
set -e

# roll out the freshly built nodepulse-server binary + web/ assets to prod.
# run from the repo root after `go build -o bin/nodepulse-server ./cmd/server/`.
# requires passwordless ssh to REMOTE_HOST as REMOTE_USER.

REMOTE_HOST="${NODEPULSE_REMOTE_HOST:-pulse.nqai.es-cloud.ru}"
REMOTE_USER="${NODEPULSE_REMOTE_USER:-deploy}"
REMOTE_DIR="${NODEPULSE_REMOTE_DIR:-/srv/nodepulse}"

echo "==> Syncing bin + web to ${REMOTE_USER}@${REMOTE_HOST}:${REMOTE_DIR}"
ssh "${REMOTE_USER}@${REMOTE_HOST}" "mkdir -p ${REMOTE_DIR}/bin ${REMOTE_DIR}/web"
scp bin/nodepulse-server "${REMOTE_USER}@${REMOTE_HOST}:${REMOTE_DIR}/bin/nodepulse-server.new"
scp -r web/public/. "${REMOTE_USER}@${REMOTE_HOST}:${REMOTE_DIR}/web/public/"
ssh "${REMOTE_USER}@${REMOTE_HOST}" "cd ${REMOTE_DIR} && mv bin/nodepulse-server.new bin/nodepulse-server && systemctl --no-block restart nodepulse-server.service && echo 'rollout: queued restart'"
