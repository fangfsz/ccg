#!/bin/sh
set -e

# Ensure data directory exists and has correct permissions
if [ ! -d "$CCNEXUS_DATA_DIR" ]; then
    mkdir -p "$CCNEXUS_DATA_DIR"
fi

# Fix permissions on data directory if running as non-root
if [ "$(id -u)" = "0" ]; then
    chown -R ccnexus:ccnexus "$CCNEXUS_DATA_DIR"
fi

exec /app/ccnexus-server "$@"
