#!/bin/sh
# Ship agent binaries for /download/ so install.sh works without GitHub access.
set -e
mkdir -p "$POLARIS_DATA_DIR/downloads"
cp -f /opt/polaris/downloads/* "$POLARIS_DATA_DIR/downloads/" 2>/dev/null || true
exec /usr/local/bin/polaris "$@"
