#!/bin/bash
#
# Command Responder for File Exchange System
# Polls command_dir for cmd-*.json files, executes commands, and writes result-*.json files
#

set -euo pipefail

COMMAND_DIR="${COMMAND_DIR:-/tmp/relay-commands}"
POLL_INTERVAL="${POLL_INTERVAL:-2}"
LOG_FILE="${LOG_FILE:-/var/log/command-responder.log}"

log() {
    echo "[$(date '+%Y-%m-%d %T')] $*" | tee -a "$LOG_FILE" 2>/dev/null || echo "[$(date '+%Y-%m-%d %T')] $*"
}

log_error() {
    echo "[$(date '+%Y-%m-%d %T')] ERROR: $*" | tee -a "$LOG_FILE" 2>/dev/null || echo "[$(date '+%Y-%m-%d %T')] ERROR: $*"
}

process_command() {
    local cmd_file="$1"
    local cmd_id

    cmd_id=$(basename "$cmd_file" | sed 's/^cmd-\(.*\)\.json$/\1/')

    if [[ -z "$cmd_id" || "$cmd_id" == "$(basename "$cmd_file")" ]]; then
        log_error "Failed to extract command ID from: $cmd_file"
        return 1
    fi

    log "Processing command: $cmd_id"

    local cmd_json
    cmd_json=$(cat "$cmd_file")

    local cmd cwd timeout
    cmd=$(echo "$cmd_json" | jq -r '.cmd // empty')
    cwd=$(echo "$cmd_json" | jq -r '.cwd // "."')
    timeout=$(echo "$cmd_json" | jq -r '.timeout // 300')

    if [[ -z "$cmd" ]]; then
        log_error "No command found in: $cmd_file"
        write_result "$cmd_id" 1 "" "No command specified"
        return 1
    fi

    if [[ ! -d "$cwd" ]]; then
        log_error "Working directory does not exist: $cwd"
        write_result "$cmd_id" 1 "" "Working directory does not exist: $cwd"
        return 1
    fi

    local stdout_file stderr_file
    stdout_file=$(mktemp)
    stderr_file=$(mktemp)

    local exit_code=0
    cd "$cwd"

    # Execute with timeout if available
    if command -v timeout >/dev/null 2>&1; then
        timeout "$timeout" sh -c "$cmd" > "$stdout_file" 2> "$stderr_file" || exit_code=$?
    else
        # Fallback: run command in background and kill after timeout
        ( sh -c "$cmd" > "$stdout_file" 2> "$stderr_file" ) &
        cmd_pid=$!
        ( sleep "$timeout"; kill -9 "$cmd_pid" 2>/dev/null ) &
        killer_pid=$!
        wait $cmd_pid || exit_code=$?
        kill -9 $killer_pid 2>/dev/null || true
    fi

    local stdout stderr
    stdout=$(cat "$stdout_file")
    stderr=$(cat "$stderr_file")

    rm -f "$stdout_file" "$stderr_file"

    write_result "$cmd_id" "$exit_code" "$stdout" "$stderr"

    rm -f "$cmd_file"

    if [[ "$exit_code" -eq 0 ]]; then
        log "Command completed successfully: $cmd_id"
    else
        log_error "Command failed with exit code $exit_code: $cmd_id"
    fi
}

write_result() {
    local cmd_id="$1"
    local exit_code="$2"
    local stdout="$3"
    local stderr="$4"

    local result_json
    result_json=$(jq -n \
        --arg id "$cmd_id" \
        --argjson exit_code "$exit_code" \
        --arg stdout "$stdout" \
        --arg stderr "$stderr" \
        '{
            id: $id,
            exit_code: $exit_code,
            stdout: $stdout,
            stderr: $stderr,
            completed_at: (now | strftime("%Y-%m-%dT%H:%M:%SZ"))
        }')

    local result_file="${COMMAND_DIR}/result-${cmd_id}.json"
    echo "$result_json" > "$result_file"
    log "Wrote result file: $result_file"
}

main() {
    log "Command responder starting..."
    log "Command directory: $COMMAND_DIR"
    log "Poll interval: $POLL_INTERVAL seconds"

    mkdir -p "$COMMAND_DIR"

    while true; do
        for cmd_file in "$COMMAND_DIR"/cmd-*.json; do
            if [[ -e "$cmd_file" ]]; then
                process_command "$cmd_file" || true
            fi
        done
        sleep "$POLL_INTERVAL"
    done
}

main "$@"