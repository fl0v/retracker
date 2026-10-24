#!/bin/sh
set -e

# Optionally refresh forwarders list on startup
should_update_forwarders() {
    case "${RETRACKER_UPDATE_FORWARDERS:-false}" in
        true|1|yes|on|enable|enabled) return 0 ;;
        *) return 1 ;;
    esac
}

# If enabled, generate the forwarders file before starting the tracker
if should_update_forwarders; then
    DEST_PATH="${RETRACKER_FORWARDS:-configs/forwarders.yml}"
    echo "Updating forwarders file at ${DEST_PATH}..."
    /app/scripts/update-forwarders.sh "${DEST_PATH}"
fi

# The binary reads configuration environment variables itself. Preserve explicit
# arguments so command-line flags keep their higher priority.
if [ "$#" -eq 0 ] || [ "${1#-}" != "$1" ]; then
    set -- ./retracker "$@"
fi
exec "$@"
