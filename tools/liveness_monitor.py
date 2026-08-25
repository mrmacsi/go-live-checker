#!/usr/bin/env python3
"""Incrementally publish progress for the standalone Go liveness scanner."""

from __future__ import annotations

import argparse
import json
import os
import time
from collections import Counter
from datetime import datetime, timezone
from pathlib import Path


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def count_lines(path: Path) -> int:
    with path.open("rb") as handle:
        return sum(1 for line in handle if line.strip())


def initial_state(total: int) -> dict:
    return {
        "machine": "M1 inactive recheck",
        "available": True,
        "status": "starting",
        "started_at": now_iso(),
        "updated_at": now_iso(),
        "total": total,
        "processed": 0,
        "remaining": total,
        "active": 0,
        "inactive": 0,
        "errors": 0,
        "ats_traceable": 0,
        "has_email": 0,
        "company_number": 0,
        "careers_page": 0,
        "platform_counts": {},
        "primary_platform_counts": {},
        "http_status_counts": {},
        "last_domain": None,
        "rate_per_second": 0.0,
        "last_batch_seconds": 0.0,
        "last_batch_rate_per_second": 0.0,
        "eta_seconds": None,
        "eta_at": None,
        "last_error": None,
        "bytes_read": 0,
    }


def write_state(path: Path, state: dict) -> None:
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(state, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    os.replace(temporary, path)


def update(state: dict, results: Path, previous_time: float, previous_processed: int) -> tuple[float, int]:
    size = results.stat().st_size if results.exists() else 0
    offset = int(state.get("bytes_read") or 0)
    if size < offset:
        offset = 0
        for key in ("processed", "active", "inactive", "errors"):
            state[key] = 0
        state["http_status_counts"] = {}

    if results.exists():
        with results.open("rb") as handle:
            handle.seek(offset)
            while True:
                line = handle.readline()
                if not line:
                    break
                if not line.strip():
                    continue
                try:
                    item = json.loads(line)
                except json.JSONDecodeError:
                    continue
                state["processed"] += 1
                state["last_domain"] = item.get("domain")
                if item.get("http_active") is True:
                    state["active"] += 1
                else:
                    state["inactive"] += 1
                if item.get("error"):
                    state["errors"] += 1
                status = item.get("http_status")
                status_key = str(status) if status is not None else "none"
                statuses = state["http_status_counts"]
                statuses[status_key] = statuses.get(status_key, 0) + 1
            state["bytes_read"] = handle.tell()

    current_time = time.monotonic()
    interval = max(0.001, current_time - previous_time)
    delta = max(0, state["processed"] - previous_processed)
    state["last_batch_seconds"] = interval
    state["last_batch_rate_per_second"] = delta / interval
    started = datetime.fromisoformat(state["started_at"]).timestamp()
    elapsed = max(0.001, time.time() - started)
    state["rate_per_second"] = state["processed"] / elapsed
    state["remaining"] = max(0, state["total"] - state["processed"])
    state["status"] = "complete" if state["processed"] >= state["total"] and state["total"] else "running"
    state["updated_at"] = now_iso()
    if state["rate_per_second"] > 0 and state["remaining"] > 0:
        state["eta_seconds"] = state["remaining"] / state["rate_per_second"]
        state["eta_at"] = datetime.fromtimestamp(time.time() + state["eta_seconds"], timezone.utc).isoformat()
    else:
        state["eta_seconds"] = None
        state["eta_at"] = None
    return current_time, state["processed"]


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--input", required=True, type=Path)
    parser.add_argument("--results", required=True, type=Path)
    parser.add_argument("--state", required=True, type=Path)
    parser.add_argument("--interval", type=float, default=5.0)
    args = parser.parse_args()

    total = count_lines(args.input)
    state = initial_state(total)
    if args.state.exists():
        try:
            loaded = json.loads(args.state.read_text(encoding="utf-8"))
            if loaded.get("total") == total:
                state.update(loaded)
        except (OSError, TypeError, ValueError, json.JSONDecodeError):
            pass
    args.state.parent.mkdir(parents=True, exist_ok=True)
    previous_time = time.monotonic()
    previous_processed = int(state.get("processed") or 0)
    while True:
        previous_time, previous_processed = update(state, args.results, previous_time, previous_processed)
        write_state(args.state, state)
        print(json.dumps({key: state[key] for key in ("status", "processed", "active", "inactive", "remaining", "rate_per_second")}), flush=True)
        if state["status"] == "complete":
            return
        time.sleep(max(0.5, args.interval))


if __name__ == "__main__":
    main()
