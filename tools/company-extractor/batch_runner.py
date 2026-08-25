#!/usr/bin/env python3
"""Resumable JSONL batch runner for the Go company extractor.

The extractor itself accepts URL arguments and returns one JSON array. This
runner keeps the queue bounded, appends one result per line, and writes a small
state file after every completed batch so an interrupted run can resume.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
from collections import Counter
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, Iterable, List, Optional


PLATFORMS = (
    "wordpress", "shopify", "webflow", "wix", "squarespace",
    "bigcommerce", "hubspot", "duda", "gohighlevel",
)


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def empty_stats(total: int) -> Dict[str, Any]:
    return {
        "machine": "",
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
        "platform_counts": {platform: 0 for platform in PLATFORMS},
        "primary_platform_counts": {platform: 0 for platform in PLATFORMS},
        "http_status_counts": {},
        "last_domain": None,
        "rate_per_second": 0.0,
        "last_batch_seconds": 0.0,
        "last_batch_rate_per_second": 0.0,
        "eta_seconds": None,
        "last_error": None,
    }


def atomic_write_json(path: Path, value: Dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    os.replace(temporary, path)


def iter_domains(path: Path) -> Iterable[str]:
    with path.open("r", encoding="utf-8", errors="replace") as handle:
        for line in handle:
            domain = line.strip()
            if domain:
                yield domain


def count_domains(path: Path) -> int:
    return sum(1 for _ in iter_domains(path))


def output_lines(path: Path) -> int:
    if not path.exists():
        return 0
    with path.open("rb") as handle:
        return sum(1 for line in handle if line.strip())


def truthy(value: Any) -> bool:
    if value is None or value is False:
        return False
    if isinstance(value, (list, tuple, set, dict)):
        return bool(value)
    if isinstance(value, str):
        text = value.strip()
        if not text:
            return False
        # Go serializes extra_info as JSON text. Treat "[]", "{}", and JSON
        # null as empty values rather than as present metadata.
        if text[:1] in "[{":
            try:
                decoded = json.loads(text)
                if isinstance(decoded, (list, dict)):
                    return bool(decoded)
            except (TypeError, ValueError, json.JSONDecodeError):
                pass
        return True
    return bool(value)


def company_number(record: Dict[str, Any]) -> bool:
    detail = record.get("company_detail") or {}
    extra = detail.get("extra_info")
    if isinstance(extra, str):
        try:
            extra = json.loads(extra)
        except (TypeError, ValueError):
            extra = {}
    if isinstance(extra, dict) and truthy(extra.get("company_number")):
        return True
    return truthy(detail.get("company_number"))


def apply_record(stats: Dict[str, Any], record: Dict[str, Any]) -> None:
    detail = record.get("company_detail") or {}
    meta = record.get("meta") or {}
    stats["processed"] += 1
    if detail.get("active") is True:
        stats["active"] += 1
    else:
        stats["inactive"] += 1
    if truthy(meta.get("error")):
        stats["errors"] += 1
    if detail.get("ats_traceable") is True:
        stats["ats_traceable"] += 1
    if truthy(detail.get("email")) or truthy(detail.get("new_emails")):
        stats["has_email"] += 1
    if company_number(record):
        stats["company_number"] += 1
    if truthy(detail.get("careers_page")):
        stats["careers_page"] += 1

    primary = meta.get("website_platform")
    if primary in stats["primary_platform_counts"]:
        stats["primary_platform_counts"][primary] += 1
    platforms = meta.get("website_platforms") or []
    if isinstance(platforms, list):
        for platform in platforms:
            if platform in stats["platform_counts"]:
                stats["platform_counts"][platform] += 1

    status = meta.get("http_status")
    status_key = str(status) if status is not None else "none"
    statuses = stats["http_status_counts"]
    statuses[status_key] = statuses.get(status_key, 0) + 1
    stats["last_domain"] = meta.get("input_url") or detail.get("website")


def rebuild_stats(output: Path, total: int, machine: str) -> Dict[str, Any]:
    stats = empty_stats(total)
    stats["machine"] = machine
    if output.exists():
        with output.open("r", encoding="utf-8", errors="replace") as handle:
            for line in handle:
                if not line.strip():
                    continue
                try:
                    apply_record(stats, json.loads(line))
                except (TypeError, ValueError, json.JSONDecodeError):
                    stats["errors"] += 1
    return stats


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--state", required=True, type=Path)
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--machine", required=True)
    parser.add_argument("--workers", type=int, default=128)
    parser.add_argument("--timeout", type=int, default=8)
    parser.add_argument("--attempts", type=int, default=2)
    parser.add_argument("--batch-size", type=int, default=2000)
    parser.add_argument("--max-batch-retries", type=int, default=2)
    return parser.parse_args()


def run() -> int:
    args = parse_args()
    if not args.input.is_file():
        raise SystemExit("input file does not exist: " + str(args.input))
    if not args.binary.is_file():
        raise SystemExit("extractor binary does not exist: " + str(args.binary))
    if min(args.workers, args.timeout, args.attempts, args.batch_size) < 1:
        raise SystemExit("workers, timeout, attempts, and batch-size must be positive")

    total = count_domains(args.input)
    processed = output_lines(args.output)
    if processed > total:
        raise SystemExit("output contains more records than input; refusing to continue")

    stats = rebuild_stats(args.output, total, args.machine)
    stats["started_at"] = stats.get("started_at") or now_iso()
    stats["status"] = "running" if processed < total else "complete"
    stats["updated_at"] = now_iso()
    stats["remaining"] = total - stats["processed"]
    session_started = time.time()
    session_start_processed = stats["processed"]
    atomic_write_json(args.state, stats)
    if processed >= total:
        return 0

    domain_iterator = iter_domains(args.input)
    for _ in range(processed):
        next(domain_iterator, None)

    args.output.parent.mkdir(parents=True, exist_ok=True)
    with args.output.open("a", encoding="utf-8", buffering=1024 * 1024) as output_handle:
        while processed < total:
            domains = []
            for _ in range(min(args.batch_size, total - processed)):
                domain = next(domain_iterator, None)
                if domain is None:
                    break
                domains.append(domain)
            if not domains:
                break

            urls = [domain if "://" in domain else "https://" + domain for domain in domains]
            command = [
                str(args.binary),
                "--timeout=" + str(args.timeout),
                "--workers=" + str(args.workers),
                "--attempts=" + str(args.attempts),
            ] + urls
            batch_started = time.monotonic()
            batch_records: Optional[List[Dict[str, Any]]] = None
            last_failure = ""
            for retry in range(args.max_batch_retries + 1):
                try:
                    completed = subprocess.run(
                        command,
                        capture_output=True,
                        text=True,
                        timeout=max(300, args.timeout * 30),
                        check=False,
                    )
                    if completed.returncode != 0:
                        last_failure = (completed.stderr or completed.stdout or "extractor failed").strip()[-2000:]
                    else:
                        parsed = json.loads(completed.stdout)
                        if isinstance(parsed, list) and len(parsed) == len(domains):
                            batch_records = parsed
                            break
                        last_failure = "extractor returned an unexpected record count"
                except (OSError, subprocess.TimeoutExpired, TypeError, ValueError, json.JSONDecodeError) as exc:
                    last_failure = str(exc)
                if retry < args.max_batch_retries:
                    time.sleep(min(30, 2 ** retry))
            if batch_records is None:
                stats["status"] = "blocked"
                stats["last_error"] = last_failure
                stats["updated_at"] = now_iso()
                atomic_write_json(args.state, stats)
                raise SystemExit("batch failed after retries: " + last_failure)

            for record in batch_records:
                output_handle.write(json.dumps(record, ensure_ascii=False, separators=(",", ":")) + "\n")
                apply_record(stats, record)
            output_handle.flush()
            os.fsync(output_handle.fileno())
            processed += len(batch_records)
            elapsed = max(0.001, time.time() - session_started)
            batch_elapsed = max(0.001, time.monotonic() - batch_started)
            session_processed = processed - session_start_processed
            stats["processed"] = processed
            stats["remaining"] = total - processed
            stats["rate_per_second"] = session_processed / elapsed
            stats["last_batch_seconds"] = batch_elapsed
            stats["last_batch_rate_per_second"] = len(batch_records) / batch_elapsed
            stats["eta_seconds"] = (total - processed) / stats["rate_per_second"] if stats["rate_per_second"] else None
            stats["status"] = "complete" if processed >= total else "running"
            stats["updated_at"] = now_iso()
            atomic_write_json(args.state, stats)
            print(
                "%s processed=%d/%d rate=%.2f/s remaining=%d"
                % (args.machine, processed, total, stats["rate_per_second"], stats["remaining"]),
                flush=True,
            )
    return 0

if __name__ == "__main__":
    run()
