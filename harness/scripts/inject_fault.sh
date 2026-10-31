#!/usr/bin/env bash
# Inject one of the proof-of-value faults (docs/roadmap.md) on THIS node, and
# undo it again. For test clusters you rent for the proof, never production:
# every fault degrades the node on purpose, and F6 makes the provider's agent
# record a real "down" event.
#
#   sudo ./inject_fault.sh --yes [--truth FILE] [--node NAME] [--dry-run] FAULT ARGS
#
#   power-cap PCT [GPU]          F1  lower the power limit PCT% below default
#   lock-clocks MHZ [GPU]        F2  lock graphics clocks at MHZ (below base)
#   link-down IFACE              F3  take one RDMA/network interface down
#   nccl-misconfig KEY=VALUE     F4  add a wrong setting to /etc/nccl.conf
#   io-throttle CGROUP DEV BPS   F5  cap a cgroup's read and write rate on device
#                                    MAJ:MIN to BPS bytes/s with cgroup v2 io.max
#   xid-drill [XID] [PCI]        F6  write a synthetic NVIDIA XID line to the kernel
#                                    log (default XID 79), for health-check timing
#   restore                          undo every fault this script applied, newest first
#   status                           list the faults currently applied
#
# Every injection and restore appends one JSON line to the truth file (the
# ground truth that capledger-harness pov-score compares detections with).
# Undo commands are kept in /run/capledger-faults, so "restore" works from
# another shell, but not after a reboot (which clears most faults anyway).
set -euo pipefail

STATE_DIR=${CAPLEDGER_FAULT_STATE:-/run/capledger-faults}
NCCL_CONF=${CAPLEDGER_NCCL_CONF:-/etc/nccl.conf}
KMSG=${CAPLEDGER_KMSG:-/dev/kmsg}
CGROUP_ROOT=${CAPLEDGER_CGROUP_ROOT:-/sys/fs/cgroup}
TRUTH=injections.jsonl
NODE=$(hostname -s 2>/dev/null || hostname)
YES=0
DRY=0

usage() { sed -n '2,23p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
die() { echo "inject_fault: $*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --yes) YES=1; shift ;;
    --dry-run) DRY=1; shift ;;
    --truth) TRUTH=$2; shift 2 ;;
    --node) NODE=$2; shift 2 ;;
    -h|--help) usage ;;
    --*) die "unknown option $1" ;;
    *) break ;;
  esac
done
[ $# -ge 1 ] || usage
FAULT=$1; shift

# run executes a command, or prints it with --dry-run.
run() {
  if [ "$DRY" = 1 ]; then
    printf 'would run:'; printf ' %q' "$@"; printf '\n'
  else
    "$@"
  fi
}

# write_to FILE TEXT: the shell redirection as one command for run().
write_to() { run sh -c 'printf "%s\n" "$2" > "$1"' sh "$1" "$2"; }

truth() { # truth ACTION FAULT PARAM
  local ts; ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  local line
  line=$(printf '{"ts":"%s","node":"%s","action":"%s","fault":"%s","param":"%s"}' "$ts" "$NODE" "$1" "$2" "$3")
  if [ "$DRY" = 1 ]; then echo "would record: $line"; else echo "$line" >> "$TRUTH"; fi
}

remember() { # remember FAULT PARAM UNDO-COMMAND...
  local f=$1 p=$2; shift 2
  if [ "$DRY" = 1 ]; then printf 'would remember undo:'; printf ' %q' "$@"; printf '\n'; return; fi
  mkdir -p "$STATE_DIR"
  printf '%s\t%s\t%s\n' "$f" "$p" "$(printf '%q ' "$@")" >> "$STATE_DIR/undo"
}

gpus() { # the GPU index given, or every GPU
  if [ -n "${1:-}" ]; then echo "$1"; return; fi
  nvidia-smi --query-gpu=index --format=csv,noheader | tr -d ' '
}

need_yes() {
  [ "$YES" = 1 ] || [ "$DRY" = 1 ] || die "this degrades the node on purpose; pass --yes on a test cluster (or --dry-run)"
}

case "$FAULT" in
  power-cap)
    need_yes
    PCT=${1:?percent below the default power limit}
    case "$PCT" in ''|*[!0-9]*) die "PCT must be a whole number" ;; esac
    [ "$PCT" -gt 0 ] && [ "$PCT" -lt 90 ] || die "PCT must be between 1 and 89"
    for g in $(gpus "${2:-}"); do
      def=$(nvidia-smi -i "$g" --query-gpu=power.default_limit --format=csv,noheader,nounits | tr -d ' ')
      new=$(awk -v d="$def" -v p="$PCT" 'BEGIN { printf "%d", d * (100 - p) / 100 }')
      run nvidia-smi -i "$g" -pl "$new"
      remember F1 "gpu$g:-$PCT%" nvidia-smi -i "$g" -pl "$def"
    done
    truth inject F1 "-$PCT%"
    ;;
  lock-clocks)
    need_yes
    MHZ=${1:?graphics clock in MHz}
    case "$MHZ" in ''|*[!0-9]*) die "MHZ must be a whole number" ;; esac
    for g in $(gpus "${2:-}"); do
      run nvidia-smi -i "$g" -lgc "$MHZ,$MHZ"
      remember F2 "gpu$g:${MHZ}MHz" nvidia-smi -i "$g" -rgc
    done
    truth inject F2 "${MHZ}MHz"
    ;;
  link-down)
    need_yes
    IFACE=${1:?interface, e.g. ib2 or the RDMA netdev of one rail}
    [ "$DRY" = 1 ] || [ -e "/sys/class/net/$IFACE" ] || die "no interface $IFACE"
    run ip link set "$IFACE" down
    remember F3 "$IFACE" ip link set "$IFACE" up
    truth inject F3 "$IFACE"
    ;;
  nccl-misconfig)
    need_yes
    KV=${1:?KEY=VALUE, e.g. NCCL_IB_GID_INDEX=7}
    case "$KV" in NCCL_*=*) ;; *) die "expected NCCL_SOMETHING=value" ;; esac
    case "$KV" in *[!A-Za-z0-9_=.:,/-]*) die "unexpected characters in $KV" ;; esac
    if [ -e "$NCCL_CONF" ]; then
      [ "$DRY" = 1 ] || { mkdir -p "$STATE_DIR"; cp -p "$NCCL_CONF" "$STATE_DIR/nccl.conf.orig"; }
      remember F4 "$KV" cp -p "$STATE_DIR/nccl.conf.orig" "$NCCL_CONF"
    else
      remember F4 "$KV" rm -f "$NCCL_CONF"
    fi
    run sh -c 'printf "%s\n" "$2" >> "$1"' sh "$NCCL_CONF" "$KV"
    truth inject F4 "$KV"
    ;;
  io-throttle)
    need_yes
    CG=${1:?cgroup path under $CGROUP_ROOT, e.g. system.slice/slurmstepd.scope}
    DEV=${2:?device MAJ:MIN, see lsblk -o NAME,MAJ:MIN}
    BPS=${3:?bytes per second}
    case "$DEV" in *[!0-9:]*|*:*:*|:*|*:) die "DEV must look like 259:0" ;; esac
    case "$BPS" in ''|*[!0-9]*) die "BPS must be a whole number" ;; esac
    case "$CG" in *..*) die "cgroup path must not contain .." ;; esac
    IOMAX="$CGROUP_ROOT/$CG/io.max"
    [ "$DRY" = 1 ] || [ -e "$IOMAX" ] || die "no $IOMAX (cgroup v2 with the io controller is needed)"
    write_to "$IOMAX" "$DEV rbps=$BPS wbps=$BPS"
    remember F5 "$CG:$DEV:$BPS" sh -c 'printf "%s\n" "$2" > "$1"' sh "$IOMAX" "$DEV rbps=max wbps=max"
    truth inject F5 "$DEV:${BPS}B/s"
    ;;
  xid-drill)
    need_yes
    XID=${1:-79}
    PCI=${2:-0000:00:00}
    case "$XID" in ''|*[!0-9]*) die "XID must be a number" ;; esac
    case "$PCI" in *[!0-9a-fA-F:.]*) die "PCI must look like 0000:3b:00" ;; esac
    write_to "$KMSG" "NVRM: Xid (PCI:$PCI): $XID, pid='<unknown>', name=<unknown>, capledger fault drill"
    truth inject F6 "xid:$XID"
    echo "drill written; when it is over, record the recovery, e.g.:"
    echo "  capledger event emit --node $NODE --type up --cause drill-end --log ... --key-dir ..."
    ;;
  restore)
    [ -s "$STATE_DIR/undo" ] || { echo "nothing to restore"; exit 0; }
    tac "$STATE_DIR/undo" | while IFS=$'\t' read -r f p cmd; do
      echo "undo $f ($p): $cmd"
      if [ "$DRY" = 1 ]; then continue; fi
      eval "$cmd" || echo "inject_fault: undo of $f ($p) failed; fix it by hand" >&2
      truth restore "$f" "$p"
    done
    [ "$DRY" = 1 ] || rm -f "$STATE_DIR/undo" "$STATE_DIR/nccl.conf.orig"
    ;;
  status)
    if [ -s "$STATE_DIR/undo" ]; then cut -f1,2 "$STATE_DIR/undo"; else echo "no faults applied"; fi
    ;;
  *) usage ;;
esac
