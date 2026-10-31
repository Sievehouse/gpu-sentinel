#!/usr/bin/env bash
# End-to-end demo on synthetic data. Needs Go 1.22+; uses a temporary directory.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="${WORK:-$(mktemp -d)}"
cd "$ROOT"

step() { printf '\n==> %s\n' "$*"; }

step "Build capledger"
go build -o "$WORK/capledger" ./cmd/capledger
CL="$WORK/capledger"
"$CL" version

step "Replay a sample kernel log through the agent; the node gets its own signing key"
LOG="$WORK/evidence-gpu-003.jsonl"
KEYS="$WORK/keys/gpu-003"
"$CL" agent --node gpu-003 --source examples/samples/kern.log --once --log "$LOG" --key-dir "$KEYS"

step "Record the repair that closes the incident (normally done by an integration)"
"$CL" event emit --log "$LOG" --key-dir "$KEYS" --node gpu-003 --type up \
  --ts 2026-10-03T06:40:00Z --cause node-reseated --source provider-health-check

step "Trust the key the agent announced, for node gpu-003 only"
"$CL" keyring import --keyring "$WORK/keyring.json" --log "$LOG" --owner "Example Neocloud" --role provider-agent
"$CL" keyring list --keyring "$WORK/keyring.json"

step "Verify the log against the keyring (hash chain, signatures, node bindings)"
"$CL" evidence verify --log "$LOG" --keyring "$WORK/keyring.json" --format text

step "Export the signed events for the SLA engine"
"$CL" events export --log "$LOG" --keyring "$WORK/keyring.json" | tee "$WORK/agent-events.jsonl"

step "AMD: replay amdgpu kernel messages; the uncorrectable RAS error becomes a down event"
"$CL" agent --vendor amd --node mi355-02 --source examples/samples/kern-amd.log --once \
  --log "$WORK/evidence-mi355-02.jsonl" --key-dir "$WORK/keys/mi355-02"
"$CL" events export --log "$WORK/evidence-mi355-02.jsonl" --pub "$WORK/keys/mi355-02/capledger.pub.pem"

step "Redaction: process names and ids that could identify a customer's job never reach the log"
"$CL" xid parse --file examples/samples/kern.log --redact | grep '"code":94'

step "Kubernetes: two saved views of the cluster (an NVSentinel fault, a lost GPU, then the repair) become signed events"
for view in 2026-10-03T0415:2026-10-03T04:15:00Z 2026-10-03T0645:2026-10-03T06:45:00Z; do
  "$CL" watch kubernetes --from "examples/kubernetes/nodes-${view%%:*}.json" --at "${view#*:}" --once \
    --down-condition '*' --expected-gpus 8 --log "$WORK/k8s-watcher.jsonl" --key-dir "$WORK/keys/k8s-watcher"
done

step "Heartbeats: a node that went silent for 27 minutes shows up as unmonitored time"
HB="$WORK/evidence-gpu-001.jsonl"
for t in 00:00 00:01 00:02 00:30 00:31; do
  printf '{"node":"gpu-001","ts":"2026-10-01T%s:00Z","intervalSeconds":60,"role":"agent"}\n' "$t" > "$WORK/hb.json"
  "$CL" evidence append --log "$HB" --key-dir "$WORK/keys/gpu-001" --kind heartbeat --payload "$WORK/hb.json" > /dev/null
done
"$CL" evidence gaps --log "$HB" --pub "$WORK/keys/gpu-001/capledger.pub.pem" --max-silence 5m \
  --since 2026-10-01T00:00:00Z --until 2026-10-01T00:31:00Z

step "Compute the October statement for the 16-node example contract (YAML)"
"$CL" sla compute --contract examples/contracts/hgx-16-node.yaml \
  --events examples/events/hgx-16-node-2026-10.jsonl --period 2026-10 \
  --out "$WORK/statement.json" --format text

step "Replay the statement independently (what the customer or an insurer would run)"
"$CL" sla verify --statement "$WORK/statement.json" \
  --contract examples/contracts/hgx-16-node.yaml --events examples/events/hgx-16-node-2026-10.jsonl

step "The same month with three unmonitored stretches, under a contract that counts them as downtime"
"$CL" sla compute --contract examples/contracts/hgx-16-node-strict.yaml \
  --events examples/events/hgx-16-node-2026-10.jsonl --events examples/events/hgx-16-node-2026-10-gaps.jsonl \
  --period 2026-10 --format text | head -7

step "Compare the provider's record with the customer's own probe"
"$CL" sla corroborate --contract examples/contracts/hgx-16-node.yaml --period 2026-10 \
  --events-a examples/events/hgx-16-node-2026-10.jsonl --events-b examples/events/hgx-16-node-2026-10-tenant.jsonl

step "Seal the statement into the evidence log and verify again"
"$CL" evidence append --log "$LOG" --key-dir "$KEYS" --kind statement --payload "$WORK/statement.json"
"$CL" evidence verify --log "$LOG" --keyring "$WORK/keyring.json" --format text

step "Freeze the statement and its inputs in a signed bundle, then check and unpack it"
"$CL" bundle create --out "$WORK/statement-2026-10.tar.gz" --type statement \
  --subject example-neocloud--example-lab--2026 --key-dir "$WORK/keys/auditor" \
  --file "$WORK/statement.json" --file contract.yaml=examples/contracts/hgx-16-node.yaml \
  --file events.jsonl=examples/events/hgx-16-node-2026-10.jsonl --file "$LOG"
"$CL" bundle verify --bundle "$WORK/statement-2026-10.tar.gz" --pub "$WORK/keys/auditor/capledger.pub.pem" \
  --extract "$WORK/unpacked"

step "Rack-scale example: three NVL72 trays overlap, so the whole rack counts as down"
"$CL" sla compute --contract examples/contracts/nvl72-2-racks.json \
  --events examples/events/nvl72-2026-10.jsonl --period 2026-10 --format text | head -8

step "Version 2 of the contract says whose clock counts: from the provider's first fault to the tenant's recovery"
"$CL" contract check --contract examples/contracts/hgx-16-node-reconciled.yaml
"$CL" sla compute --contract examples/contracts/hgx-16-node-reconciled.yaml \
  --events examples/events/hgx-16-node-2026-10-both.jsonl --period 2026-10 --format text | sed -n '1,6p;8,12p'

step "Each party signs the October statement; it counts as agreed only when both accept"
KR="$WORK/keyring.json"
for p in provider:"Example Neocloud" customer:"Example Lab"; do
  role=${p%%:*}; owner=${p#*:}
  "$CL" keygen --out "$WORK/keys/$role-signer" > /dev/null
  "$CL" keyring add --keyring "$KR" --pub "$WORK/keys/$role-signer/capledger.pub.pem" --owner "$owner" --role "$role"
  "$CL" sla sign --statement "$WORK/statement.json" --party "$role" --key-dir "$WORK/keys/$role-signer"
done
"$CL" sla verify --statement "$WORK/statement.json" --keyring "$KR" --require provider,customer \
  --signature "$WORK/statement.provider.sig.json" --signature "$WORK/statement.customer.sig.json"

step "Run a ledger for the contract: both parties push, a witness checkpoints, a financier reads the feed"
PROBE="$WORK/probe-gpu-003.jsonl"
"$CL" event emit --log "$PROBE" --key-dir "$WORK/keys/probe-gpu-003" --role tenant-probe --node gpu-003 --type down \
  --ts 2026-10-03T04:13:00Z --cause tenant:gpu-missing:7/8 --source tenant-probe > /dev/null
"$CL" keyring import --keyring "$KR" --log "$PROBE" --owner "Example Lab" --role tenant-probe
"$CL" keygen --out "$WORK/keys/insurer-signer" > /dev/null
"$CL" keyring add --keyring "$KR" --pub "$WORK/keys/insurer-signer/capledger.pub.pem" --owner "Example Insurer" --role financier
parties=()
for p in example-neocloud:provider:provider-agent example-lab:customer:tenant-probe example-insurer:financier:; do
  IFS=: read -r name role write <<< "$p"
  hash=$("$CL" ledger token --out "$WORK/$name.token" | awk '/tokenSha256/ {print $2}')
  roles=""; [ -n "$write" ] && roles=", \"writeRoles\": [\"$write\"]"
  parties+=("{\"name\": \"$name\", \"role\": \"$role\", \"tokenSha256\": \"$hash\"$roles}")
done
cat > "$WORK/ledger.json" <<JSON
{"apiVersion": "capacity-ledger/v1alpha1", "kind": "LedgerConfig", "contract": "example-neocloud--example-lab--2026",
 "parties": [$(IFS=,; echo "${parties[*]}")]}
JSON
"$CL" ledger serve --config "$WORK/ledger.json" --keyring "$KR" --data "$WORK/ledger-data" \
  --key-dir "$WORK/keys/witness" --listen 127.0.0.1:0 2> "$WORK/ledger.log" &
LEDGER_PID=$!
trap 'kill "$LEDGER_PID" 2>/dev/null || true' EXIT
for _ in $(seq 50); do grep -q 'http://' "$WORK/ledger.log" && break; sleep 0.1; done
URL=$(grep -o 'http://127.0.0.1:[0-9]*' "$WORK/ledger.log")
as() { local who=$1; shift; "$CL" ledger "$@" --server "$URL" --token-file "$WORK/$who.token"; }
as example-neocloud push --log "$LOG"
as example-lab push --log "$PROBE"
as example-lab pull --all --out-dir "$WORK/pulled"
as example-lab checkpoint --create --out "$WORK/checkpoint.json"
as example-lab status --witness-pub "$WORK/witness.pub.pem"
"$CL" keyring add --keyring "$KR" --pub "$WORK/witness.pub.pem" --owner "Ledger operator" --role witness
"$CL" evidence verify --keyring "$KR" --checkpoint "$WORK/checkpoint.json" --format text \
  --log "$WORK/pulled/evidence-gpu-003.jsonl" --log "$WORK/pulled/probe-gpu-003.jsonl"
as example-neocloud publish --statement "$WORK/statement.json" --signature "$WORK/statement.provider.sig.json"
as example-lab publish --signature "$WORK/statement.customer.sig.json"
as example-neocloud consent --financier example-insurer --party provider --key-dir "$WORK/keys/provider-signer"
as example-lab consent --financier example-insurer --party customer --key-dir "$WORK/keys/customer-signer"
as example-insurer feed --out-dir "$WORK/feed"
kill "$LEDGER_PID"; wait "$LEDGER_PID" 2>/dev/null || true

printf '\nDone. Files are in %s\n' "$WORK"
