#!/usr/bin/env bash
# Build and deploy a resumable Go company-extractor job to any SSH host.
#
# Example:
#   tools/company-extractor/deploy_remote_runner.sh \
#     --host c3-highcpu-8 \
#     --input company-extractor/live-dashboard/m1-input-last-100k-c3-highcpu-8.txt \
#     --job c3-m1-tail-100k \
#     --workers 128 --timeout 8 --attempts 2 --batch-size 2000
#
# The input is copied through a temporary remote filename and atomically
# renamed. The Python runner appends JSONL and rewrites its state file after
# every batch, so rerunning the same command resumes from completed output.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HOST=""
INPUT=""
JOB=""
REMOTE_ROOT="~/crawler"
WORKERS=128
TIMEOUT=8
ATTEMPTS=2
BATCH_SIZE=2000
MACHINE=""
NO_START=0

usage() {
  sed -n '1,24p' "$0"
  cat <<'EOF'

Options:
  --host HOST             SSH alias or host (required)
  --input FILE             local newline-delimited input (required)
  --job NAME               remote job name (required; letters, digits, . _ -)
  --remote-root PATH      remote workspace (default: ~/crawler)
  --workers N              extractor workers (default: 128)
  --timeout N              per-request timeout seconds (default: 8)
  --attempts N             request attempts (default: 2)
  --batch-size N           bounded batch size (default: 2000)
  --machine NAME           label in state (default: JOB)
  --no-start               deploy only; do not launch the runner
  -h, --help               show this help
EOF
}

while (($#)); do
  case "$1" in
    --host) HOST="$2"; shift 2 ;;
    --input) INPUT="$2"; shift 2 ;;
    --job) JOB="$2"; shift 2 ;;
    --remote-root) REMOTE_ROOT="$2"; shift 2 ;;
    --workers) WORKERS="$2"; shift 2 ;;
    --timeout) TIMEOUT="$2"; shift 2 ;;
    --attempts) ATTEMPTS="$2"; shift 2 ;;
    --batch-size) BATCH_SIZE="$2"; shift 2 ;;
    --machine) MACHINE="$2"; shift 2 ;;
    --no-start) NO_START=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ -z "$HOST" || -z "$INPUT" || -z "$JOB" ]]; then
  echo "--host, --input, and --job are required" >&2
  usage >&2
  exit 2
fi
if [[ ! -f "$INPUT" ]]; then
  echo "input file does not exist: $INPUT" >&2
  exit 1
fi
if [[ ! "$JOB" =~ ^[A-Za-z0-9._-]+$ ]]; then
  echo "job must contain only letters, digits, dot, underscore, and hyphen" >&2
  exit 2
fi
if [[ -z "$MACHINE" ]]; then MACHINE="$JOB"; fi

BUILD_DIR="$(mktemp -d "${TMPDIR:-/tmp}/company-extractor-build.XXXXXX")"
trap 'rm -rf "$BUILD_DIR"' EXIT

echo "Building Linux amd64 extractor..."
(cd "$ROOT_DIR" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$BUILD_DIR/company-extractor" ./tools/company-extractor)

REMOTE_HOME="$(ssh "$HOST" 'printf %s "$HOME"')"
if [[ "$REMOTE_ROOT" == "~/"* ]]; then
  REMOTE_ROOT="$REMOTE_HOME/${REMOTE_ROOT#~/}"
fi

INPUT_REMOTE="$REMOTE_ROOT/data/$JOB.input.txt"
OUTPUT_REMOTE="$REMOTE_ROOT/data/$JOB.jsonl"
STATE_REMOTE="$REMOTE_ROOT/data/$JOB.state.json"
LOG_REMOTE="$REMOTE_ROOT/logs/$JOB.log"
RUNNER_REMOTE="$REMOTE_ROOT/bin/batch_runner.py"
BINARY_REMOTE="$REMOTE_ROOT/bin/company-extractor"

echo "Preparing $HOST..."
ssh "$HOST" "mkdir -p '$REMOTE_ROOT/data' '$REMOTE_ROOT/logs' '$REMOTE_ROOT/bin'"

echo "Copying binary, runner, and input..."
scp "$BUILD_DIR/company-extractor" "$HOST:$BINARY_REMOTE.tmp.$$"
scp "$ROOT_DIR/tools/company-extractor/batch_runner.py" "$HOST:$RUNNER_REMOTE.tmp.$$"
scp "$INPUT" "$HOST:$INPUT_REMOTE.tmp.$$"
ssh "$HOST" "mv '$BINARY_REMOTE.tmp.$$' '$BINARY_REMOTE' && chmod 755 '$BINARY_REMOTE' && mv '$RUNNER_REMOTE.tmp.$$' '$RUNNER_REMOTE' && chmod 755 '$RUNNER_REMOTE' && mv '$INPUT_REMOTE.tmp.$$' '$INPUT_REMOTE'"

echo "Remote input: $INPUT_REMOTE"
ssh "$HOST" "wc -l '$INPUT_REMOTE'; sha256sum '$INPUT_REMOTE'"

if ((NO_START)); then
  echo "Deployed without starting the runner."
  exit 0
fi

echo "Starting/resuming $JOB with workers=$WORKERS timeout=$TIMEOUT attempts=$ATTEMPTS batch_size=$BATCH_SIZE"
ssh "$HOST" "nohup python3 '$RUNNER_REMOTE' --input '$INPUT_REMOTE' --output '$OUTPUT_REMOTE' --state '$STATE_REMOTE' --binary '$BINARY_REMOTE' --machine '$MACHINE' --workers '$WORKERS' --timeout '$TIMEOUT' --attempts '$ATTEMPTS' --batch-size '$BATCH_SIZE' > '$LOG_REMOTE' 2>&1 < /dev/null & echo \$!"
echo "Started. State: $HOST:$STATE_REMOTE"
