#!/usr/bin/env bash
# Atomically take a queue tail, deploy it to a remote Go extractor, start or
# resume the job, and register its paths in the live dashboard configuration.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
QUEUE=""
TAIL_COUNT=100000
HOST=""
JOB=""
LABEL=""
WORKERS=128
TIMEOUT=8
ATTEMPTS=2
BATCH_SIZE=2000
CONFIG="$SCRIPT_DIR/crawler-servers.json"
M1_AGENT="$HOME/Library/LaunchAgents/com.sponsorcompanies.m1-runner.plist"
PAUSE_M1=1
REUSE_INPUT=0

usage() {
  cat <<'EOF'
Usage:
  dispatch_job.sh --queue FILE --host HOST --job NAME [options]

Required:
  --queue FILE             M1 newline-delimited queue
  --host HOST              SSH alias or host
  --job NAME               unique job name

Options:
  --tail N                 number of domains to move from queue end (default: 100000)
  --label TEXT             dashboard label (default: JOB)
  --workers N              Go workers (default: 128)
  --timeout N              request timeout seconds (default: 8)
  --attempts N             request attempts (default: 2)
  --batch-size N           bounded batch size (default: 2000)
  --dashboard-config FILE  dashboard server registry JSON
  --m1-agent FILE          M1 LaunchAgent to pause/resume during queue move
  --no-m1-pause            do not pause the M1 runner (unsafe for a live queue)
  --reuse-input            deploy an already extracted local input file
  -h, --help               show this help
EOF
}

while (($#)); do
  case "$1" in
    --queue) QUEUE="$2"; shift 2 ;;
    --tail) TAIL_COUNT="$2"; shift 2 ;;
    --host) HOST="$2"; shift 2 ;;
    --job) JOB="$2"; shift 2 ;;
    --label) LABEL="$2"; shift 2 ;;
    --workers) WORKERS="$2"; shift 2 ;;
    --timeout) TIMEOUT="$2"; shift 2 ;;
    --attempts) ATTEMPTS="$2"; shift 2 ;;
    --batch-size) BATCH_SIZE="$2"; shift 2 ;;
    --dashboard-config) CONFIG="$2"; shift 2 ;;
    --m1-agent) M1_AGENT="$2"; shift 2 ;;
    --no-m1-pause) PAUSE_M1=0; shift ;;
    --reuse-input) REUSE_INPUT=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ -z "$QUEUE" || -z "$HOST" || -z "$JOB" ]]; then
  echo "--queue, --host, and --job are required" >&2
  usage >&2
  exit 2
fi
if [[ ! -f "$QUEUE" ]]; then
  echo "queue file does not exist: $QUEUE" >&2
  exit 1
fi
if [[ ! "$JOB" =~ ^[A-Za-z0-9._-]+$ ]]; then
  echo "job must contain only letters, digits, dot, underscore, and hyphen" >&2
  exit 2
fi
if [[ ! "$TAIL_COUNT" =~ ^[0-9]+$ || "$TAIL_COUNT" -lt 1 ]]; then
  echo "--tail must be a positive integer" >&2
  exit 2
fi
if [[ -z "$LABEL" ]]; then LABEL="$JOB"; fi

QUEUE_DIR="$(cd "$(dirname "$QUEUE")" && pwd)"
QUEUE="$(cd "$(dirname "$QUEUE")" && pwd)/$(basename "$QUEUE")"
INPUT_FILE="$QUEUE_DIR/$JOB.input.txt"
LOCK_DIR="$QUEUE.dispatch.lock"
QUEUE_TMP="$QUEUE.tmp.$$"
INPUT_TMP="$INPUT_FILE.tmp.$$"
M1_WAS_LOADED=0

pause_m1() {
  if ((PAUSE_M1)) && [[ -f "$M1_AGENT" ]]; then
    local domain="gui/$(id -u)"
    if launchctl print "$domain/com.sponsorcompanies.m1-runner" >/dev/null 2>&1; then
      launchctl bootout "$domain" "$M1_AGENT"
      M1_WAS_LOADED=1
      echo "Paused M1 runner while changing its queue."
    fi
  fi
}

resume_m1() {
  if ((M1_WAS_LOADED)); then
    local domain="gui/$(id -u)"
    launchctl bootstrap "$domain" "$M1_AGENT"
    launchctl enable "$domain/com.sponsorcompanies.m1-runner" >/dev/null 2>&1 || true
    echo "Resumed M1 runner from the trimmed queue."
  fi
}

if ! mkdir "$LOCK_DIR" 2>/dev/null; then
  echo "queue is already being dispatched: $LOCK_DIR" >&2
  exit 1
fi
cleanup() {
  rm -f "$QUEUE_TMP" "$INPUT_TMP"
  rmdir "$LOCK_DIR" 2>/dev/null || true
  resume_m1
}
trap cleanup EXIT

if [[ -e "$INPUT_FILE" && "$REUSE_INPUT" -eq 0 ]]; then
  echo "refusing to overwrite existing extracted input: $INPUT_FILE" >&2
  exit 1
fi

if ((REUSE_INPUT)); then
  INPUT_COUNT="$(wc -l < "$INPUT_FILE" | tr -d ' ')"
  if [[ "$INPUT_COUNT" != "$TAIL_COUNT" ]]; then
    echo "existing input has $INPUT_COUNT lines; expected $TAIL_COUNT" >&2
    exit 1
  fi
  NEW_QUEUE_COUNT="$(wc -l < "$QUEUE" | tr -d ' ')"
  echo "Reusing verified extracted input; queue currently has $NEW_QUEUE_COUNT domains."
else
  pause_m1
  TOTAL="$(wc -l < "$QUEUE" | tr -d ' ')"
  if ((TOTAL < TAIL_COUNT)); then
    echo "queue has $TOTAL lines; cannot move $TAIL_COUNT" >&2
    exit 1
  fi
  KEEP=$((TOTAL - TAIL_COUNT))

  echo "Extracting last $TAIL_COUNT domains to: $INPUT_FILE"
  tail -n "$TAIL_COUNT" "$QUEUE" > "$INPUT_TMP"
  mv "$INPUT_TMP" "$INPUT_FILE"

  if ((KEEP > 0)); then
    head -n "$KEEP" "$QUEUE" > "$QUEUE_TMP"
  else
    : > "$QUEUE_TMP"
  fi
  mv "$QUEUE_TMP" "$QUEUE"

  NEW_QUEUE_COUNT="$(wc -l < "$QUEUE" | tr -d ' ')"
  INPUT_COUNT="$(wc -l < "$INPUT_FILE" | tr -d ' ')"
  if [[ "$NEW_QUEUE_COUNT" != "$KEEP" || "$INPUT_COUNT" != "$TAIL_COUNT" ]]; then
    echo "verification failed: queue=$NEW_QUEUE_COUNT expected=$KEEP input=$INPUT_COUNT expected=$TAIL_COUNT" >&2
    exit 1
  fi
fi

echo "Moved $INPUT_COUNT domains; queue now has $NEW_QUEUE_COUNT domains."
echo "Input SHA256: $(shasum -a 256 "$INPUT_FILE" | awk '{print $1}')"

"$SCRIPT_DIR/deploy_remote_runner.sh" \
  --host "$HOST" \
  --input "$INPUT_FILE" \
  --job "$JOB" \
  --machine "$LABEL" \
  --workers "$WORKERS" \
  --timeout "$TIMEOUT" \
  --attempts "$ATTEMPTS" \
  --batch-size "$BATCH_SIZE"

python3 - "$CONFIG" "$JOB" "$LABEL" "$HOST" "$JOB" <<'PY'
import json
import os
import sys
from pathlib import Path

config_path, key, label, host, job = sys.argv[1:]
path = Path(config_path)
try:
    loaded = json.loads(path.read_text(encoding="utf-8")) if path.exists() else []
except (OSError, TypeError, ValueError, json.JSONDecodeError):
    loaded = []
if isinstance(loaded, dict):
    loaded = loaded.get("servers", [])
if not isinstance(loaded, list):
    loaded = []

entry = {
    "key": key,
    "label": label,
    "mode": "auto",
    "host": host,
    "input": f"~/crawler/data/{job}.input.txt",
    "results": f"~/crawler/data/{job}.jsonl",
    "log": f"~/crawler/logs/{job}.log",
    "stats": f"~/crawler/data/{job}.state.json",
}
updated = [item for item in loaded if not isinstance(item, dict) or item.get("key") != key]
updated.append(entry)
path.parent.mkdir(parents=True, exist_ok=True)
temporary = path.with_suffix(path.suffix + ".tmp")
temporary.write_text(json.dumps(updated, indent=2) + "\n", encoding="utf-8")
os.replace(temporary, path)
print(f"Dashboard registered: {key} -> {host}")
PY

echo "Done. Dashboard will load the new server on its next refresh."
