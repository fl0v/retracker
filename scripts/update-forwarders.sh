#!/bin/bash

# Script to download tracker lists, merge with optional additional trackers, and regenerate forwarders.yml
# Supports both HTTP and UDP trackers, removes duplicates

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
LISTS_DIR="${SCRIPT_DIR}/lists"

# Destination forwarders file (supports absolute or project-relative paths)
DEST_PATH="${1:-configs/forwarders.yml}"
if [[ "${DEST_PATH}" = /* ]]; then
    OUTPUT_FILE="${DEST_PATH}"
else
    OUTPUT_FILE="${PROJECT_ROOT}/${DEST_PATH}"
fi

# Tracker list sources are loaded from configs/trackers-lists.txt (one entry per line),
# unless overridden via env var TRACKER_LISTS_FILE.
# Supported formats (per line):
#   - URL|filename
#   - URL                      (filename will be derived)
# Default: configs/trackers-lists.txt
TRACKER_LISTS_FILE="${TRACKER_LISTS_FILE:-configs/trackers-lists.txt}"

# Optional additional trackers file (one tracker per line; can also contain mixed formats),
# unless overridden via env var ADDITIONAL_TRACKERS_FILE.
# Default: configs/trackers.txt
ADDITIONAL_TRACKERS_FILE="${ADDITIONAL_TRACKERS_FILE:-configs/trackers.txt}"

resolve_path() {
    # Resolve path: absolute stays absolute; relative becomes project-root-relative.
    local p="$1"
    if [[ -z "$p" ]]; then
        printf '%s' ""
        return
    fi
    if [[ "$p" = /* ]]; then
        printf '%s' "$p"
    else
        printf '%s' "${PROJECT_ROOT}/${p}"
    fi
}

TRACKER_LISTS_FILE="$(resolve_path "$TRACKER_LISTS_FILE")"
ADDITIONAL_TRACKERS_FILE="$(resolve_path "$ADDITIONAL_TRACKERS_FILE")"

trim() {
    # Trim leading/trailing whitespace from stdin
    local s
    s="$(cat)"
    s="${s#"${s%%[![:space:]]*}"}"
    s="${s%"${s##*[![:space:]]}"}"
    printf '%s' "$s"
}

derive_filename_from_url() {
    local url="$1"
    local base
    base="$(basename "${url%%\?*}")"
    if [[ -n "$base" ]] && [[ "$base" != "/" ]] && [[ "$base" != "." ]]; then
        printf '%s\n' "$base"
        return
    fi
    # Fallback: convert URL into a safe-ish filename
    printf '%s\n' "$(echo "$url" | sed -E 's#^[a-zA-Z]+://##; s#[^a-zA-Z0-9._-]+#_#g').txt"
}

load_tracker_lists() {
    local -a lists=()
    if [[ ! -f "$TRACKER_LISTS_FILE" ]]; then
        echo "Warning: Tracker lists file not found at ${TRACKER_LISTS_FILE}" >&2
        echo "         Create it with one tracker-list URL per line (optionally URL|filename)." >&2
        printf '%s\n' "${lists[@]}"
        return
    fi

    while IFS= read -r raw || [[ -n "$raw" ]]; do
        # Strip comments and trim whitespace
        local line url filename
        line="${raw%%#*}"
        line="$(printf '%s' "$line" | trim)"
        [[ -z "$line" ]] && continue

        if [[ "$line" == *"|"* ]]; then
            url="${line%%|*}"
            filename="${line#*|}"
            url="$(printf '%s' "$url" | trim)"
            filename="$(printf '%s' "$filename" | trim)"
        else
            url="$line"
            filename="$(derive_filename_from_url "$url")"
        fi

        [[ -z "$url" ]] && continue
        [[ -z "$filename" ]] && filename="$(derive_filename_from_url "$url")"

        lists+=("${url}|${filename}")
    done < "$TRACKER_LISTS_FILE"

    printf '%s\n' "${lists[@]}"
}

mapfile -t TRACKER_LISTS < <(load_tracker_lists)

# Create lists directory if it doesn't exist
mkdir -p "$LISTS_DIR"
# Create destination directory if it doesn't exist
mkdir -p "$(dirname "${OUTPUT_FILE}")"

echo "Downloading tracker lists to ${LISTS_DIR}..."

# Download online lists (with timeout and error handling)
if (( ${#TRACKER_LISTS[@]} == 0 )); then
    echo "Note: No tracker list sources configured (TRACKER_LISTS is empty)."
    echo "      Add entries to ${TRACKER_LISTS_FILE} to enable downloading."
else
    for list_entry in "${TRACKER_LISTS[@]}"; do
        IFS='|' read -r url filename <<< "$list_entry"
        output_file="${LISTS_DIR}/${filename}"
        echo "  Downloading ${filename}..."
        curl -s -L --max-time 10 "$url" -o "$output_file" 2>/dev/null || echo "Warning: Failed to download ${filename} from ${url}" >&2
    done
fi

# Function to extract HTTP and UDP trackers from a file
extract_trackers() {
    local file="$1"
    if [[ ! -f "$file" ]] || [[ ! -s "$file" ]]; then
        return
    fi
    
    # Extract HTTP/HTTPS/UDP URLs
    # Handle formats: one per line (most common), comma-separated, space-separated, JSON arrays
    # First, extract lines starting with http://, https://, or udp://
    grep -ihE '^(https?|udp)://' "$file" 2>/dev/null | \
    sed 's/[[:space:]]*$//' | \
    # Also extract URLs from within lines (for comma/space separated or JSON)
    cat - <(grep -ihEo '(https?|udp)://[^[:space:],\[\]"]+' "$file" 2>/dev/null) | \
    sed 's/[[:space:]]*$//' | \
    sed 's/,$//' | \
    sed 's/\]$//' | \
    sed 's/\[$//' | \
    sed 's/"$//' | \
    sed 's/^[[:space:]]*"//' | \
    sed 's/[[:space:]]*$//' | \
    grep -iE '^(https?|udp)://' | \
    sed 's|/$||' | \
    sed 's|/announce$||' | \
    awk '{if (length($0) > 0) print $0 "/announce"}' | \
    sort -u
}

# Extract HTTP and UDP trackers from all downloaded files
echo "Extracting HTTP and UDP trackers..."
ALL_TRACKERS="${LISTS_DIR}/_all.txt"
> "$ALL_TRACKERS"

for file in "${LISTS_DIR}"/*.txt; do
    filename=$(basename "$file")
    # Skip internal processing files (underscore-prefixed)
    [[ "$filename" == _* ]] && continue
    
    if [[ -f "$file" ]] && [[ -s "$file" ]]; then
        extract_trackers "$file" >> "$ALL_TRACKERS" 2>/dev/null || true
    fi
done

# Add additional trackers from configs/trackers.txt (optional)
if [[ -f "$ADDITIONAL_TRACKERS_FILE" ]] && [[ -s "$ADDITIONAL_TRACKERS_FILE" ]]; then
    echo "Adding additional trackers from ${ADDITIONAL_TRACKERS_FILE}..."
    extract_trackers "$ADDITIONAL_TRACKERS_FILE" >> "$ALL_TRACKERS" 2>/dev/null || true
else
    echo "Note: Additional trackers file not found or empty at ${ADDITIONAL_TRACKERS_FILE}"
    echo "Creating empty file for future use..."
    mkdir -p "$(dirname "${ADDITIONAL_TRACKERS_FILE}")"
    touch "$ADDITIONAL_TRACKERS_FILE"
fi

# Normalize and deduplicate trackers with smart host-based deduplication
echo "Normalizing and deduplicating trackers..."
NORMALIZED="${LISTS_DIR}/_normalized.txt"
> "$NORMALIZED"

if [[ -s "$ALL_TRACKERS" ]]; then
    while IFS= read -r tracker || [[ -n "$tracker" ]]; do
        # Skip empty lines
        [[ -z "$tracker" ]] && continue
        
        # Normalize: convert to lowercase, remove trailing slashes
        normalized=$(echo "$tracker" | tr '[:upper:]' '[:lower:]' | sed 's|/$||')
        
        # Only process if it's HTTP/HTTPS/UDP and not empty
        if [[ "$normalized" =~ ^(https?|udp):// ]] && [[ -n "$normalized" ]]; then
            # Ensure /announce suffix if no path or empty path
            if [[ "$normalized" =~ ^(https?|udp)://[^/]+$ ]] || [[ "$normalized" =~ ^(https?|udp)://[^/]+/$ ]]; then
                normalized="${normalized%/}/announce"
            elif [[ ! "$normalized" =~ / ]]; then
                normalized="${normalized}/announce"
            fi
            echo "$normalized" >> "$NORMALIZED"
        fi
    done < "$ALL_TRACKERS"
fi

# Function to extract host from URL (for grouping)
extract_host() {
    local url="$1"
    # Remove protocol
    url="${url#*://}"
    # Extract host (everything up to first / or :)
    local host="${url%%[:/]*}"
    printf '%s' "$host"
}

# Function to parse URL components
parse_url() {
    local url="$1"
    local protocol host port path
    
    # Extract protocol
    if [[ "$url" =~ ^(https?|udp):// ]]; then
        protocol="${BASH_REMATCH[1]}"
    else
        return 1
    fi
    
    # Remove protocol
    url="${url#*://}"
    
    # Extract host and port
    if [[ "$url" =~ ^([^:/]+)(:([0-9]+))?(/.*)?$ ]]; then
        host="${BASH_REMATCH[1]}"
        port="${BASH_REMATCH[3]}"
        path="${BASH_REMATCH[4]}"
        
        # Set default ports if not specified
        if [[ -z "$port" ]]; then
            if [[ "$protocol" == "http" ]]; then
                port="80"
            elif [[ "$protocol" == "https" ]]; then
                port="443"
            elif [[ "$protocol" == "udp" ]]; then
                port="80"
            fi
        fi
        
        # Set default path if not specified
        if [[ -z "$path" ]]; then
            path="/announce"
        fi
        
        printf '%s|%s|%s|%s' "$protocol" "$host" "$port" "$path"
        return 0
    fi
    
    return 1
}

# Smart deduplication: group by host and apply preference rules
UNIQUE_TRACKERS="${LISTS_DIR}/_unique.txt"
if [[ -s "$NORMALIZED" ]]; then
    # Group trackers by host
    declare -A host_groups
    
    while IFS= read -r tracker || [[ -n "$tracker" ]]; do
        [[ -z "$tracker" ]] && continue
        host=$(extract_host "$tracker")
        if [[ -n "$host" ]]; then
            host_groups["$host"]="${host_groups["$host"]}${host_groups["$host"]:+$'\n'}$tracker"
        fi
    done < "$NORMALIZED"
    
    # For each host, select the best tracker
    > "$UNIQUE_TRACKERS"
    for host in "${!host_groups[@]}"; do
        # Check if we have both UDP and HTTP (first pass)
        has_udp=false
        has_http=false
        while IFS= read -r tracker || [[ -n "$tracker" ]]; do
            [[ -z "$tracker" ]] && continue
            parsed=$(parse_url "$tracker")
            if [[ -n "$parsed" ]]; then
                IFS='|' read -r protocol _ _ _ <<< "$parsed"
                if [[ "$protocol" == "udp" ]]; then
                    has_udp=true
                else
                    has_http=true
                fi
            fi
        done <<< "${host_groups["$host"]}"
        
        # Select best tracker (second pass)
        best_tracker=""
        best_protocol=""
        best_port=""
        best_path=""
        
        while IFS= read -r tracker || [[ -n "$tracker" ]]; do
            [[ -z "$tracker" ]] && continue
            parsed=$(parse_url "$tracker")
            if [[ -z "$parsed" ]]; then
                continue
            fi
            
            IFS='|' read -r protocol _ current_port current_path <<< "$parsed"
            # Normalize path for comparison (remove leading/trailing slashes)
            current_path_norm="${current_path#/}"
            current_path_norm="${current_path_norm%/}"
            
            # Rule 1: If we have both UDP and HTTP, prefer UDP
            if [[ "$has_udp" == true ]] && [[ "$has_http" == true ]]; then
                if [[ "$protocol" != "udp" ]]; then
                    continue
                fi
            fi
            
            # If no best tracker yet, use this one
            if [[ -z "$best_tracker" ]]; then
                best_tracker="$tracker"
                best_protocol="$protocol"
                best_port="$current_port"
                best_path="$current_path_norm"
                continue
            fi
            
            # Rule 2: If different paths and one is "announce", prefer that one
            if [[ "$current_path_norm" == "announce" ]]; then
                if [[ "$best_path" != "announce" ]]; then
                    best_tracker="$tracker"
                    best_protocol="$protocol"
                    best_port="$current_port"
                    best_path="$current_path_norm"
                    continue
                fi
            elif [[ "$best_path" == "announce" ]]; then
                continue
            fi
            
            # Rule 3: If different ports, prefer 6969 or 1337
            if [[ "$current_port" == "6969" ]] || [[ "$current_port" == "1337" ]]; then
                if [[ "$best_port" != "6969" ]] && [[ "$best_port" != "1337" ]]; then
                    best_tracker="$tracker"
                    best_protocol="$protocol"
                    best_port="$current_port"
                    best_path="$current_path_norm"
                    continue
                fi
            elif [[ "$best_port" == "6969" ]] || [[ "$best_port" == "1337" ]]; then
                continue
            fi
            
            # If all else equal, keep the first one we found
        done <<< "${host_groups["$host"]}"
        
        if [[ -n "$best_tracker" ]]; then
            echo "$best_tracker" >> "$UNIQUE_TRACKERS"
        fi
    done
    
    # Sort the final list
    if [[ -s "$UNIQUE_TRACKERS" ]]; then
        sort -u "$UNIQUE_TRACKERS" -o "$UNIQUE_TRACKERS"
    fi
else
    > "$UNIQUE_TRACKERS"
fi

# Count trackers by protocol
TRACKER_COUNT=$(wc -l < "$UNIQUE_TRACKERS" 2>/dev/null | tr -d ' ' || echo "0")
HTTP_COUNT=$(grep -c '^https\?://' "$UNIQUE_TRACKERS" 2>/dev/null || echo "0")
UDP_COUNT=$(grep -c '^udp://' "$UNIQUE_TRACKERS" 2>/dev/null || echo "0")
echo "Found ${TRACKER_COUNT} unique trackers (${HTTP_COUNT} HTTP, ${UDP_COUNT} UDP)"

# Generate forwarders.yml
echo "Generating ${OUTPUT_FILE}..."
{
    echo "# Forwarder configuration for retracker"
    echo "# Auto-generated by update-forwarders.sh on $(date -u +"%Y-%m-%d %H:%M:%S UTC")"
    echo "# Total trackers: ${TRACKER_COUNT} (${HTTP_COUNT} HTTP, ${UDP_COUNT} UDP)"
    echo "# retracker supports both HTTP and UDP trackers (BEP 15)"
    echo ""
    
    if [[ -s "$UNIQUE_TRACKERS" ]]; then
        while IFS= read -r tracker || [[ -n "$tracker" ]]; do
            [[ -z "$tracker" ]] && continue
            echo "- uri: ${tracker}"
        done < "$UNIQUE_TRACKERS"
    fi
} > "$OUTPUT_FILE"

echo "Successfully generated ${OUTPUT_FILE} with ${TRACKER_COUNT} trackers"
echo "Done!"
