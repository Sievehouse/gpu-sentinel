#!/usr/bin/env bash
# Record Slurm node availability transitions as signed SLA events.
#
# Polls `sinfo` and appends a "down" event when a node enters an unavailable
# state and an "up" event when it returns. This is how repairs close the
# incidents that the kernel-log agent opens. Run it on the Slurm controller
# (or any host with sinfo) under a service manager such as systemd.
#
# The watcher speaks for every node, so its key is enrolled without node
# bindings: capledger keyring import --log slurm-events.jsonl --role provider-integration
# --bind-nodes=false ...
#
# State mapping (edit UNAVAILABLE to match your contract's definition):
#   down, drained, draining, fail, failing, maint, reboot, inval, or any state
#   ending in "*" (not responding)            -> down
#   anything else (idle, allocated, mixed...)  -> up
#
# Environment:
#   CAPLEDGER_BIN       path to capledger            (default: capledger)
#   CAPLEDGER_LOG       evidence log to append to    (default: /var/lib/capledger/slurm-events.jsonl)
#   CAPLEDGER_KEY_DIR   the watcher's own key        (default: /var/lib/capledger/watcher-keys; created on first use)
#   CAPLEDGER_KEY       a private key file to use instead of CAPLEDGER_KEY_DIR
#   CAPLEDGER_STATE     last seen states (tsv)       (default: /var/lib/capledger/slurm-node-state.tsv)
#   CAPLEDGER_INTERVAL  seconds between polls        (default: 30)
#   CAPLEDGER_ONCE      set to 1 to poll once and exit (for testing)
set -euo pipefail

BIN=${CAPLEDGER_BIN:-capledger}
LOG=${CAPLEDGER_LOG:-/var/lib/capledger/slurm-events.jsonl}
if [[ -n "${CAPLEDGER_KEY:-}" ]]; then
  SIGNER=(--key "$CAPLEDGER_KEY")
else
  SIGNER=(--key-dir "${CAPLEDGER_KEY_DIR:-/var/lib/capledger/watcher-keys}")
fi
STATE=${CAPLEDGER_STATE:-/var/lib/capledger/slurm-node-state.tsv}
INTERVAL=${CAPLEDGER_INTERVAL:-30}
UNAVAILABLE='^(down|drained|draining|drain|fail|failing|maint|reboot|inval)|\*$'

touch "$STATE"

is_down() { [[ "$1" =~ $UNAVAILABLE ]]; }

poll() {
  local now node state reason prev
  now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  # One line per node; sinfo -N repeats nodes that sit in several partitions.
  sinfo -N -h -o '%N|%T|%E' | sort -u -t'|' -k1,1 > "$STATE.new"
  while IFS='|' read -r node state reason; do
    prev=$(awk -F'|' -v n="$node" '$1 == n { print $2 }' "$STATE")
    [[ "$prev" == "$state" ]] && continue
    if is_down "$state" && { [[ -z "$prev" ]] || ! is_down "$prev"; }; then
      "$BIN" event emit --log "$LOG" "${SIGNER[@]}" --node "$node" --type down \
        --cause "slurm:${state}" --note "${reason}" --ts "$now" --source slurm-state-watcher
    elif [[ -n "$prev" ]] && is_down "$prev" && ! is_down "$state"; then
      "$BIN" event emit --log "$LOG" "${SIGNER[@]}" --node "$node" --type up \
        --cause "slurm:${state}" --ts "$now" --source slurm-state-watcher
    fi
  done < "$STATE.new"
  mv "$STATE.new" "$STATE"
}

while true; do
  poll
  [[ "${CAPLEDGER_ONCE:-0}" == "1" ]] && exit 0
  sleep "$INTERVAL"
done
