#!/bin/sh
set -e

# Build command arguments from environment variables
ARGS=""

# Optionally refresh forwarders list on startup
should_update_forwarders() {
    case "${RETRACKER_UPDATE_FORWARDERS:-false}" in
        true|1|yes|on|enable|enabled) return 0 ;;
        *) return 1 ;;
    esac
}

# If enabled, generate forwarders file before building args
if should_update_forwarders; then
    DEST_PATH="${RETRACKER_FORWARDS:-configs/forwarders.yml}"
    echo "Updating forwarders file at ${DEST_PATH}..."
    /app/scripts/update-forwarders.sh "${DEST_PATH}"
fi

# -c (config) - Configuration file path
if [ -n "${RETRACKER_CONFIG:-}" ]; then
    ARGS="${ARGS} -c ${RETRACKER_CONFIG}"
fi

# -l (listen) - HTTP listen address:port (overrides config file)
if [ -n "${RETRACKER_LISTEN:-}" ]; then
    ARGS="${ARGS} -l ${RETRACKER_LISTEN}"
fi

# -u (UDP listen) - UDP listen address:port (empty to disable)
if [ -n "${RETRACKER_UDP_LISTEN:-}" ]; then
    ARGS="${ARGS} -u ${RETRACKER_UDP_LISTEN}"
fi

# -f (forwards) - Load forwards from YAML file
if [ -n "${RETRACKER_FORWARDS:-}" ]; then
    ARGS="${ARGS} -f ${RETRACKER_FORWARDS}"
fi

# -d (debug) - Debug mode
if [ "${RETRACKER_DEBUG:-false}" = "true" ]; then
    ARGS="${ARGS} -d"
fi

# Execute retracker with built arguments
exec ./retracker ${ARGS}
