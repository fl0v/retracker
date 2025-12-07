#!/bin/sh
set -e

# Build command arguments from environment variables
ARGS=""

# -l (listen) - HTTP listen address:port
if [ -n "${RETRACKER_LISTEN:-}" ]; then
    ARGS="${ARGS} -l ${RETRACKER_LISTEN}"
else
    ARGS="${ARGS} -l :6969"
fi

# -u (UDP listen) - UDP listen address:port (empty to disable)
if [ -n "${RETRACKER_UDP_LISTEN:-}" ]; then
    ARGS="${ARGS} -u ${RETRACKER_UDP_LISTEN}"
fi

# -a (age) - Keep 'n' minutes peer in memory
if [ -n "${RETRACKER_AGE:-}" ]; then
    ARGS="${ARGS} -a ${RETRACKER_AGE}"
else
    ARGS="${ARGS} -a 180"
fi

# -d (debug) - Debug mode
if [ "${RETRACKER_DEBUG:-false}" = "true" ]; then
    ARGS="${ARGS} -d"
fi

# -x (x-real-ip) - Get RemoteAddr from X-Real-IP header
if [ "${RETRACKER_X_REAL_IP:-false}" = "true" ]; then
    ARGS="${ARGS} -x"
fi

# -f (forwards) - Load forwards from YAML file
if [ -n "${RETRACKER_FORWARDS:-}" ]; then
    ARGS="${ARGS} -f ${RETRACKER_FORWARDS}"
fi

# -t (forward timeout) - Timeout (sec) for forward requests
if [ -n "${RETRACKER_FORWARD_TIMEOUT:-}" ]; then
    ARGS="${ARGS} -t ${RETRACKER_FORWARD_TIMEOUT}"
else
    ARGS="${ARGS} -t 30"
fi

# -w (forwarder workers) - Number of workers for parallel forwarder processing
if [ -n "${RETRACKER_FORWARDER_WORKERS:-}" ]; then
    ARGS="${ARGS} -w ${RETRACKER_FORWARDER_WORKERS}"
else
    ARGS="${ARGS} -w 10"
fi

# -p (prometheus) - Enable Prometheus metrics
if [ "${RETRACKER_PROMETHEUS:-false}" = "true" ]; then
    ARGS="${ARGS} -p"
fi

# -i (announce interval) - Announce response interval (sec)
if [ -n "${RETRACKER_ANNOUNCE_INTERVAL:-}" ]; then
    ARGS="${ARGS} -i ${RETRACKER_ANNOUNCE_INTERVAL}"
else
    ARGS="${ARGS} -i 30"
fi

# -s (stats interval) - Statistics print interval (sec)
if [ -n "${RETRACKER_STATS_INTERVAL:-}" ]; then
    ARGS="${ARGS} -s ${RETRACKER_STATS_INTERVAL}"
else
    ARGS="${ARGS} -s 60"
fi

# Execute retracker with built arguments
exec ./retracker ${ARGS}
