#!/usr/bin/env bash
# Tenant side on Slurm: run "capledger probe" on every node of the job, next
# to the workload, so the customer keeps its own signed record of when the
# GPUs it pays for were usable.
#
# The probe runs as an overlapping job step that asks for the node's GPUs, so
# it sees the devices the workload sees. Check once on your cluster that
# "srun --overlap --gpus-per-node=8 nvidia-smi -L" inside a job lists every
# GPU: device cgroups are configured differently from site to site.
#
# Evidence goes to a shared directory the tenant controls, one log and one
# key per node. Copy it off the cluster when the job ends.
#
#SBATCH --job-name=train-with-probe
#SBATCH --nodes=4
#SBATCH --ntasks-per-node=8
#SBATCH --gpus-per-node=8
#SBATCH --time=24:00:00
set -euo pipefail

EVIDENCE=${EVIDENCE:-$HOME/capledger-evidence}
GPUS=${SLURM_GPUS_PER_NODE:-8}
mkdir -p "$EVIDENCE"

# One probe per node, in the background, for the life of the job. Add
# --vendor amd on AMD Instinct nodes, and --bench-cmd/--bench-floor to check
# performance as well as presence.
export EVIDENCE GPUS
srun --overlap --ntasks-per-node=1 --gpus-per-node="$GPUS" --kill-on-bad-exit=0 \
  bash -c 'exec capledger probe --node "$SLURMD_NODENAME" --expected-gpus "$GPUS" --interval 60s \
    --log "$EVIDENCE/probe-$SLURMD_NODENAME.jsonl" --key-dir "$EVIDENCE/keys-$SLURMD_NODENAME"' &
PROBES=$!

# The workload.
srun python train.py

kill "$PROBES" 2>/dev/null || true
wait "$PROBES" 2>/dev/null || true
