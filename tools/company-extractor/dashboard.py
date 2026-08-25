#!/usr/bin/env python3
"""Small read-only live dashboard for company-extractor runners."""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
import os
import shutil
import shlex
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any, Dict, Optional
from urllib.parse import parse_qs, urlparse


PLATFORMS = (
    "wordpress", "shopify", "webflow", "wix", "squarespace",
    "bigcommerce", "hubspot", "duda", "gohighlevel",
)


def blank(machine: str) -> Dict[str, Any]:
    return {
        "machine": machine,
        "available": False,
        "status": "offline",
        "total": 0,
        "processed": 0,
        "remaining": 0,
        "active": 0,
        "inactive": 0,
        "errors": 0,
        "ats_traceable": 0,
        "has_email": 0,
        "company_number": 0,
        "careers_page": 0,
        "platform_counts": {p: 0 for p in PLATFORMS},
        "primary_platform_counts": {p: 0 for p in PLATFORMS},
        "http_status_counts": {},
        "rate_per_second": 0.0,
        "last_batch_seconds": 0.0,
        "last_batch_rate_per_second": 0.0,
        "elapsed_seconds": 0.0,
        "eta_seconds": None,
        "eta_at": None,
        "started_at": None,
        "updated_at": None,
        "last_error": None,
        "resources": blank_resources(),
    }


def blank_resources() -> Dict[str, Any]:
    return {
        "extractor_cpu_percent": 0.0,
        "extractor_ram_mb": 0.0,
        "memory_used_gb": 0.0,
        "memory_total_gb": 0.0,
        "results_json_mb": 0.0,
        "results_json_bytes": 0,
        "error": None,
    }


def read_json(path: Path) -> Optional[Dict[str, Any]]:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, TypeError, ValueError, json.JSONDecodeError):
        return None


def crawler_specs(args: argparse.Namespace) -> list[Dict[str, Any]]:
    """Load crawler definitions so new jobs can register without code edits."""
    config_path = getattr(args, "crawler_config", None)
    if config_path:
        try:
            loaded = json.loads(Path(config_path).read_text(encoding="utf-8"))
            if isinstance(loaded, dict):
                loaded = loaded.get("servers", [])
            if isinstance(loaded, list):
                valid = []
                for item in loaded:
                    if not isinstance(item, dict):
                        continue
                    required = ("key", "label", "host", "input", "results", "log", "stats")
                    if all(str(item.get(field, "")).strip() for field in required):
                        valid.append({**item, "mode": item.get("mode", "auto")})
                if valid:
                    return valid
        except (OSError, TypeError, ValueError, json.JSONDecodeError):
            pass
    return [
        {"key": "crawler", "label": "Crawler server", "mode": args.crawler_mode, "host": args.crawler_host, "input": args.crawler_input, "results": args.crawler_results, "log": args.crawler_log, "stats": args.crawler_stats},
        {"key": "c3", "label": "C3 highcpu-8", "mode": args.crawler2_mode, "host": args.crawler2_host, "input": args.crawler2_input, "results": args.crawler2_results, "log": args.crawler2_log, "stats": args.crawler2_stats},
    ]


def remote_json(host: str, path: str, *, timeout: float = 4, connect_timeout: int = 2) -> Optional[Dict[str, Any]]:
    try:
        result = subprocess.run(
            ["ssh", "-o", f"ConnectTimeout={connect_timeout}", "-o", "BatchMode=yes", host, "cat", path],
            capture_output=True,
            text=True,
            timeout=timeout,
            check=False,
        )
        if result.returncode == 0:
            return json.loads(result.stdout)
    except (OSError, subprocess.TimeoutExpired, TypeError, ValueError, json.JSONDecodeError):
        pass
    return None


def remote_signature(host: str, paths: list[str]) -> Optional[str]:
    script = "import json, os, sys; print(json.dumps([(p, os.path.getsize(os.path.expanduser(p)), int(os.path.getmtime(os.path.expanduser(p))*1000000000)) if os.path.exists(os.path.expanduser(p)) else (p, None, None) for p in sys.argv[1:]]))"
    command = "python3 -c " + shlex.quote(script) + " " + " ".join(shlex.quote(path) for path in paths)
    try:
        completed = subprocess.run(
            ["ssh", "-o", "ConnectTimeout=2", "-o", "BatchMode=yes", host, command],
            capture_output=True,
            text=True,
            timeout=4,
            check=False,
        )
        if completed.returncode == 0:
            return completed.stdout.strip()
    except (OSError, subprocess.TimeoutExpired):
        pass
    return None


def remote_crawler_progress(host: str, input_path: str, results_path: str, log_path: str) -> Optional[Dict[str, Any]]:
    """Read only cheap counters when parsing the full remote JSONL times out."""
    script = r'''
import json, os, re, sys

def lines(path):
    try:
        with open(os.path.expanduser(path), "rb") as handle:
            return sum(1 for line in handle if line.strip())
    except OSError:
        return 0

input_path, results_path, log_path = sys.argv[1:4]
total = lines(input_path)
processed = lines(results_path)
updated = None
try:
    with open(os.path.expanduser(log_path), encoding="utf-8", errors="replace") as handle:
        for line in handle.readlines()[-50:][::-1]:
            match = re.search(r"completed=(\d+)/(\d+)", line)
            if match:
                processed = max(processed, int(match.group(1)))
                updated = line.split(" ", 1)[0]
                break
except OSError:
    pass
print(json.dumps({
    "machine": "Crawler server",
    "available": total > 0 or processed > 0,
    "status": "complete" if total > 0 and processed >= total else "running",
    "total": total,
    "processed": processed,
    "remaining": max(0, total - processed),
    "updated_at": updated,
    "last_error": "Detailed remote statistics temporarily unavailable",
}))
'''
    command = "python3 -c " + shlex.quote(script) + " " + " ".join(shlex.quote(path) for path in (input_path, results_path, log_path))
    try:
        completed = subprocess.run(
            ["ssh", "-o", "ConnectTimeout=2", "-o", "BatchMode=yes", host, command],
            capture_output=True,
            text=True,
            timeout=7,
            check=False,
        )
        if completed.returncode == 0:
            return json.loads(completed.stdout)
    except (OSError, subprocess.TimeoutExpired, TypeError, ValueError, json.JSONDecodeError):
        pass
    return None


RESOURCE_SCRIPT = r'''
import json, os, re, subprocess, sys

def number(value):
    try:
        return float(value)
    except (TypeError, ValueError):
        return 0.0

def main():
    result = {
        "extractor_cpu_percent": 0.0,
        "extractor_ram_mb": 0.0,
        "memory_used_gb": 0.0,
        "memory_total_gb": 0.0,
        "results_json_mb": 0.0,
        "results_json_bytes": 0,
        "error": None,
    }
    try:
        ps = subprocess.run(
            ["ps", "-axo", "pid=,ppid=,%cpu=,rss=,command="],
            capture_output=True, text=True, check=False,
        )
        for line in ps.stdout.splitlines():
            fields = line.strip().split(None, 4)
            if len(fields) < 5 or "company-extractor --timeout=8" not in fields[4]:
                continue
            result["extractor_cpu_percent"] += number(fields[2])
            result["extractor_ram_mb"] += number(fields[3]) / 1024.0

        total = int(subprocess.check_output(["sysctl", "-n", "hw.memsize"], text=True).strip())
        result["memory_total_gb"] = total / (1024 ** 3)
        vm = subprocess.check_output(["vm_stat"], text=True, stderr=subprocess.DEVNULL)
        page_match = re.search(r"page size of (\d+) bytes", vm)
        page_size = int(page_match.group(1)) if page_match else 4096
        pages = {}
        for line in vm.splitlines():
            match = re.match(r"([^:]+):\s+(\d+)\.", line)
            if match:
                pages[match.group(1).strip().lower()] = int(match.group(2))
        reclaimable = sum(pages.get(key, 0) for key in ("pages free", "pages inactive", "pages speculative"))
        result["memory_used_gb"] = max(0.0, (total - reclaimable * page_size) / (1024 ** 3))

        result_path = sys.argv[1] if len(sys.argv) > 1 else ""
        if result_path and os.path.isfile(result_path):
            result["results_json_bytes"] = os.path.getsize(result_path)
            result["results_json_mb"] = result["results_json_bytes"] / (1024 ** 2)
    except Exception as exc:
        result["error"] = str(exc)
    print(json.dumps(result))

main()
'''


def local_resources(result_path: Path) -> Dict[str, Any]:
    try:
        completed = subprocess.run(
            ["python3", "-c", RESOURCE_SCRIPT, str(result_path)],
            capture_output=True,
            text=True,
            timeout=3,
            check=False,
        )
        if completed.returncode == 0:
            return json.loads(completed.stdout)
        return {**blank_resources(), "error": completed.stderr.strip()[-500:]}
    except (OSError, subprocess.TimeoutExpired, TypeError, ValueError, json.JSONDecodeError) as exc:
        return {**blank_resources(), "error": str(exc)}


def remote_resources(host: str, result_path: str) -> Dict[str, Any]:
    import base64
    encoded = base64.b64encode(RESOURCE_SCRIPT.encode("utf-8")).decode("ascii")
    command = "python3 -c \"import base64;exec(base64.b64decode('" + encoded + "'))\" " + shlex.quote(result_path)
    try:
        completed = subprocess.run(
            ["ssh", "-o", "ConnectTimeout=2", "-o", "BatchMode=yes", host, command],
            capture_output=True,
            text=True,
            timeout=5,
            check=False,
        )
        if completed.returncode == 0:
            return json.loads(completed.stdout)
        return {**blank_resources(), "error": completed.stderr.strip()[-500:]}
    except (OSError, subprocess.TimeoutExpired, TypeError, ValueError, json.JSONDecodeError) as exc:
        return {**blank_resources(), "error": str(exc)}


CRAWLER_STATS_SCRIPT = r'''
import json, os, re, sys
from datetime import datetime, timezone

PLATFORMS = ("wordpress", "shopify", "webflow", "wix", "squarespace", "bigcommerce", "hubspot", "duda", "gohighlevel")

def truthy(value):
    if value is None or value is False:
        return False
    if isinstance(value, (list, tuple, set, dict)):
        return bool(value)
    if isinstance(value, str):
        text = value.strip()
        if not text:
            return False
        if text[:1] in "[{":
            try:
                decoded = json.loads(text)
                if isinstance(decoded, (list, dict)):
                    return bool(decoded)
            except (TypeError, ValueError, json.JSONDecodeError):
                pass
        return True
    return bool(value)

def company_number(record):
    detail = record.get("company_detail") or {}
    extra = detail.get("extra_info")
    if isinstance(extra, str):
        try:
            extra = json.loads(extra)
        except (TypeError, ValueError, json.JSONDecodeError):
            extra = {}
    if isinstance(extra, dict) and truthy(extra.get("company_number")):
        return True
    return truthy(detail.get("company_number"))

def iso_from_log(line):
    value = line.split(" ", 1)[0].strip()
    try:
        return datetime.fromisoformat(value.replace("Z", "+00:00")).isoformat()
    except ValueError:
        return None

def main():
    input_path, results_path, log_path = [os.path.expanduser(value) for value in sys.argv[1:4]]
    result = {
        "machine": "Crawler server",
        "available": True,
        "status": "running",
        "total": 0,
        "processed": 0,
        "remaining": 0,
        "active": 0,
        "inactive": 0,
        "errors": 0,
        "ats_traceable": 0,
        "has_email": 0,
        "company_number": 0,
        "careers_page": 0,
        "platform_counts": {p: 0 for p in PLATFORMS},
        "primary_platform_counts": {p: 0 for p in PLATFORMS},
        "http_status_counts": {},
        "rate_per_second": 0.0,
        "last_batch_seconds": 0.0,
        "last_batch_rate_per_second": 0.0,
        "started_at": None,
        "updated_at": None,
        "last_domain": None,
        "last_error": None,
    }
    try:
        with open(input_path, encoding="utf-8", errors="replace") as source:
            result["total"] = sum(1 for line in source if line.strip())
    except OSError:
        result["available"] = False
        result["status"] = "offline"
        print(json.dumps(result))
        return

    try:
        with open(results_path, encoding="utf-8", errors="replace") as source:
            for line in source:
                if not line.strip():
                    continue
                try:
                    record = json.loads(line)
                except (TypeError, ValueError, json.JSONDecodeError):
                    result["errors"] += 1
                    continue
                detail = record.get("company_detail") or {}
                meta = record.get("meta") or {}
                result["processed"] += 1
                if detail.get("active") is True:
                    result["active"] += 1
                else:
                    result["inactive"] += 1
                if meta.get("error") is not None:
                    result["errors"] += 1
                if detail.get("ats_traceable") is True:
                    result["ats_traceable"] += 1
                if truthy(detail.get("email")) or truthy(detail.get("new_emails")):
                    result["has_email"] += 1
                if company_number(record):
                    result["company_number"] += 1
                if truthy(detail.get("careers_page")):
                    result["careers_page"] += 1
                primary = meta.get("website_platform")
                if primary in result["primary_platform_counts"]:
                    result["primary_platform_counts"][primary] += 1
                for platform in meta.get("website_platforms") or []:
                    if platform in result["platform_counts"]:
                        result["platform_counts"][platform] += 1
                status = meta.get("http_status")
                status_key = str(status) if status is not None else "none"
                result["http_status_counts"][status_key] = result["http_status_counts"].get(status_key, 0) + 1
                result["last_domain"] = meta.get("input_url") or detail.get("website")
    except OSError:
        result["available"] = False
        result["status"] = "offline"

    log_lines = []
    try:
        with open(log_path, encoding="utf-8", errors="replace") as log:
            log_lines = [line.strip() for line in log if line.strip()]
    except OSError:
        pass
    starts = [line for line in log_lines if " start " in (" " + line)]
    if starts:
        result["started_at"] = iso_from_log(starts[-1])
    completions = []
    for line in log_lines:
        match = re.search(r"completed=(\d+)/(\d+).*results=(\d+)", line)
        if match:
            completions.append((iso_from_log(line), int(match.group(1))))
    if completions:
        result["updated_at"] = completions[-1][0]
        if len(completions) >= 2 and completions[-1][0] and completions[-2][0]:
            first = datetime.fromisoformat(completions[-2][0]).timestamp()
            last = datetime.fromisoformat(completions[-1][0]).timestamp()
            seconds = max(0.001, last - first)
            delta = max(0, completions[-1][1] - completions[-2][1])
            result["last_batch_seconds"] = seconds
            result["last_batch_rate_per_second"] = delta / seconds
            result["rate_per_second"] = delta / seconds
    if result["processed"] >= result["total"] and result["total"] > 0:
        result["status"] = "complete"
    result["remaining"] = max(0, result["total"] - result["processed"])
    print(json.dumps(result))

main()
'''


def remote_crawler_stats(host: str, input_path: str, results_path: str, log_path: str) -> Optional[Dict[str, Any]]:
    import base64
    encoded = base64.b64encode(CRAWLER_STATS_SCRIPT.encode("utf-8")).decode("ascii")
    command = "python3 -c \"import base64;exec(base64.b64decode('" + encoded + "'))\" " + " ".join(shlex.quote(path) for path in (input_path, results_path, log_path))
    try:
        completed = subprocess.run(
            ["ssh", "-o", "ConnectTimeout=3", "-o", "BatchMode=yes", host, command],
            capture_output=True,
            text=True,
            timeout=15,
            check=False,
        )
        if completed.returncode == 0:
            return json.loads(completed.stdout)
    except (OSError, subprocess.TimeoutExpired, TypeError, ValueError, json.JSONDecodeError):
        pass
    return None


def add_map(target: Dict[str, int], source: Any) -> None:
    if not isinstance(source, dict):
        return
    for key, value in source.items():
        try:
            target[key] = target.get(key, 0) + int(value)
        except (TypeError, ValueError):
            continue


def parse_timestamp(value: Any) -> Optional[float]:
    if not value or not isinstance(value, str):
        return None
    try:
        return datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()
    except ValueError:
        return None


def add_timing(state: Dict[str, Any], now: Optional[float] = None) -> Dict[str, Any]:
    now = time.time() if now is None else now
    started = parse_timestamp(state.get("started_at"))
    state["elapsed_seconds"] = max(0.0, now - started) if started is not None else 0.0
    rate = float(state.get("rate_per_second") or 0)
    remaining = int(state.get("remaining") or 0)
    eta_seconds = remaining / rate if rate > 0 and remaining > 0 else None
    state["eta_seconds"] = eta_seconds
    state["eta_at"] = datetime.fromtimestamp(now + eta_seconds, timezone.utc).isoformat() if eta_seconds is not None else None
    return state


def combined(*machines: Dict[str, Any]) -> Dict[str, Any]:
    total: Dict[str, Any] = {
        "machine": "combined",
        "available": bool(any(machine.get("available") for machine in machines)),
        "status": "complete" if machines and all(machine.get("status") == "complete" for machine in machines if machine.get("available")) else "running",
        "total": 0,
        "processed": 0,
        "remaining": 0,
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
        "rate_per_second": 0.0,
        "last_batch_seconds": 0.0,
        "last_batch_rate_per_second": 0.0,
        "elapsed_seconds": 0.0,
        "eta_seconds": None,
        "eta_at": None,
        "started_at": None,
    }
    for machine in machines:
        if not machine.get("available"):
            continue
        for key in ("total", "processed", "remaining", "active", "inactive", "errors", "ats_traceable", "has_email", "company_number", "careers_page"):
            total[key] += int(machine.get(key) or 0)
        total["rate_per_second"] += float(machine.get("rate_per_second") or 0)
        total["last_batch_rate_per_second"] += float(machine.get("last_batch_rate_per_second") or 0)
        add_map(total["platform_counts"], machine.get("platform_counts"))
        add_map(total["primary_platform_counts"], machine.get("primary_platform_counts"))
        add_map(total["http_status_counts"], machine.get("http_status_counts"))
    if total["rate_per_second"] > 0:
        total["eta_seconds"] = total["remaining"] / total["rate_per_second"]
    starts = [parse_timestamp(machine.get("started_at")) for machine in machines]
    starts = [value for value in starts if value is not None]
    if starts:
        now = time.time()
        total["started_at"] = datetime.fromtimestamp(min(starts), timezone.utc).isoformat()
        total["elapsed_seconds"] = max(0.0, now - min(starts))
        if total["eta_seconds"] is not None:
            total["eta_at"] = datetime.fromtimestamp(now + total["eta_seconds"], timezone.utc).isoformat()
    return total


class Dashboard:
    def __init__(self, args: argparse.Namespace) -> None:
        self.args = args
        self.lock = threading.Lock()
        self.pushed_m4: Optional[Dict[str, Any]] = None
        self.pushed_m4_at = 0.0
        self.crawler_cache: Dict[str, Dict[str, Any]] = {}
        self.crawler_signatures: Dict[str, Optional[str]] = {}
        self.data: Dict[str, Any] = {
            "m1": blank("M1"),
            "m4": blank("M4"),
            "combined": blank("combined"),
            "machine_keys": ["m1", "m4"],
        }
        self.refresh()

    def accept_push(self, state: Dict[str, Any]) -> None:
        state = dict(state)
        state["machine"] = "M4"
        state["pushed_at"] = time.time()
        with self.lock:
            self.pushed_m4 = state
            self.pushed_m4_at = time.time()

    def refresh(self) -> None:
        now = time.time()
        m1 = read_json(self.args.m1_state) or blank("M1")
        m1["available"] = True
        if self.args.m4_mode == "disabled":
            m4 = blank("M4")
        else:
            with self.lock:
                pushed = dict(self.pushed_m4) if self.pushed_m4 is not None else None
                pushed_age = now - self.pushed_m4_at
            if pushed is not None and pushed_age <= self.args.push_ttl:
                m4 = pushed
            else:
                m4 = remote_json(self.args.m4_host, self.args.m4_state) or blank("M4")
        m1["machine"] = "M1"
        m4["machine"] = "M4"
        if m4.get("status") != "offline":
            m4["available"] = True
        m1["resources"] = local_resources(self.args.m1_state.with_name("m1-results.jsonl"))
        if m4.get("pushed_at"):
            m4["resources"] = m4.get("resources") or blank_resources()
        else:
            m4["resources"] = remote_resources(self.args.m4_host, str(Path(self.args.m4_state).with_name("m4-results.jsonl")))
        crawlers = {}
        for spec in crawler_specs(self.args):
            key = str(spec["key"])
            label = str(spec["label"])
            mode = str(spec.get("mode", "auto"))
            host = str(spec["host"])
            input_path = str(spec["input"])
            results_path = str(spec["results"])
            log_path = str(spec["log"])
            stats_path = str(spec["stats"])
            if mode == "disabled":
                crawler = blank(label)
            else:
                crawler_paths = [input_path, results_path, log_path, stats_path]
                signature = remote_signature(host, crawler_paths)
                cached = self.crawler_cache.get(key)
                if signature is not None and signature == self.crawler_signatures.get(key) and cached is not None:
                    crawler = dict(cached)
                else:
                    # Prefer the runner's sidecar stats. It is updated after
                    # every bounded batch and avoids parsing a large JSONL.
                    detailed = remote_json(host, stats_path, timeout=10, connect_timeout=6)
                    if detailed is not None:
                        crawler = detailed
                    else:
                        progress = remote_crawler_progress(host, input_path, results_path, log_path)
                        if cached is not None:
                            crawler = dict(cached)
                            if progress is not None:
                                for field in ("available", "status", "total", "processed", "remaining", "updated_at", "last_error"):
                                    if field in progress:
                                        crawler[field] = progress[field]
                        else:
                            crawler = blank(label)
                            if progress is not None:
                                crawler.update(progress)
                    if crawler.get("available"):
                        self.crawler_signatures[key] = signature
                        self.crawler_cache[key] = dict(crawler)
                crawler["machine"] = label
                if crawler.get("available"):
                    crawler["resources"] = remote_resources(host, results_path)
            crawler["machine"] = label
            crawlers[key] = crawler
        add_timing(m1, now)
        add_timing(m4, now)
        for crawler in crawlers.values():
            add_timing(crawler, now)
        machine_keys = ["m1", "m4", *crawlers.keys()]
        with self.lock:
            self.data = {"m1": m1, "m4": m4, **crawlers, "combined": combined(m1, m4, *crawlers.values()), "machine_keys": machine_keys, "refreshed_at": time.time()}

    def loop(self) -> None:
        while True:
            self.refresh()
            time.sleep(self.args.refresh)


HTML = r'''<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Company extractor live dashboard</title>
<style>
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:#f4f6f8;color:#17202a;margin:0;padding:24px}
h1{margin:0 0 6px}.muted{color:#64748b}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(210px,1fr));gap:12px;margin:18px 0}
.card{background:#fff;border-radius:12px;padding:16px;box-shadow:0 1px 5px #0001}.label{font-size:12px;color:#64748b;text-transform:uppercase;letter-spacing:.05em}.value{font-size:28px;font-weight:700;margin-top:4px}
table{border-collapse:collapse;width:100%;background:#fff;border-radius:12px;overflow:hidden}th,td{padding:10px 12px;border-bottom:1px solid #e5e7eb;text-align:left}th{background:#eef2f7}.right{text-align:right}.bar{height:10px;background:#e5e7eb;border-radius:8px;overflow:hidden}.fill{height:100%;background:#2563eb}.section{margin-top:22px}.status-running{color:#2563eb}.status-complete{color:#16a34a}.status-offline{color:#dc2626}
</style></head><body><h1>Company extractor</h1><div class="muted" id="updated">Loading live state…</div>
<div class="grid" id="summary"></div><div class="section"><h2>Machine progress</h2><table><thead><tr><th>Machine</th><th>Progress</th><th>Elapsed</th><th>Active</th><th>Inactive</th><th>Avg domains/sec</th><th>Last batch/sec</th><th>ETA</th><th>Est. finish</th><th>Status</th></tr></thead><tbody id="machines"></tbody></table></div>
<div class="section"><h2>Machine resources</h2><table><thead><tr><th>Machine</th><th>Go CPU</th><th>Go RAM</th><th>System RAM</th><th>Extracted JSONL</th></tr></thead><tbody id="resources"></tbody></table></div>
<div class="section"><h2>Company metadata</h2><table><thead><tr><th>Machine</th><th>ATS traceable</th><th>Has email</th><th>Company number</th><th>Careers page</th><th>Errors</th></tr></thead><tbody id="metadata"></tbody></table></div>
<div class="section"><h2>Detected platforms</h2><table><thead id="platform-head"></thead><tbody id="platforms"></tbody></table></div>
<script>
const fmt=n=>Number(n||0).toLocaleString(); const pct=(a,b)=>b?((a/b)*100).toFixed(1)+'%':'0.0%';
const duration=s=>{s=Math.max(0,Number(s||0));const h=Math.floor(s/3600),m=Math.floor((s%3600)/60);if(h)return h+'h '+m+'m';return m+'m'};
const eta=s=>{s=Number(s||0);if(!s)return '—';if(s<3600)return Math.round(s/60)+' min';return (s/3600).toFixed(1)+' h'};
const finish=at=>at?new Date(at).toLocaleString():'—';
const mb=n=>Number(n||0).toFixed(1)+' MB';
function card(label,value){return `<div class="card"><div class="label">${label}</div><div class="value">${fmt(value)}</div></div>`}
function render(d){const c=d.combined;const keys=d.machine_keys||['m1','m4','crawler','c3'];document.getElementById('updated').textContent='Last dashboard refresh: '+new Date().toLocaleString()+keys.filter(k=>!d[k].available).map(k=>' · '+d[k].machine+' unavailable').join('');
document.getElementById('summary').innerHTML=[card('Processed',c.processed),card('Remaining',c.remaining),card('Active',c.active),card('Inactive',c.inactive),card('ATS traceable',c.ats_traceable),card('Has email',c.has_email),card('Company number',c.company_number),card('Careers page',c.careers_page)].join('');
document.getElementById('machines').innerHTML=keys.map(k=>{let x=d[k];if(!x.available)return `<tr><td>${x.machine}</td><td colspan="9">N/A — machine unavailable</td></tr>`;return `<tr><td>${x.machine}</td><td>${fmt(x.processed)} / ${fmt(x.total)}<div class="bar"><div class="fill" style="width:${pct(x.processed,x.total)}"></div></div></td><td>${duration(x.elapsed_seconds)}</td><td>${fmt(x.active)}</td><td>${fmt(x.inactive)}</td><td>${Number(x.rate_per_second||0).toFixed(2)}/s</td><td>${Number(x.last_batch_rate_per_second||0).toFixed(2)}/s</td><td>${eta(x.eta_seconds)}</td><td>${finish(x.eta_at)}</td><td class="status-${x.status}">${x.status}</td></tr>`}).join('');
document.getElementById('resources').innerHTML=keys.map(k=>{let x=d[k];if(!x.available)return `<tr><td>${x.machine}</td><td colspan="4">N/A — machine unavailable</td></tr>`;let r=x.resources||{};return `<tr><td>${x.machine}</td><td>${Number(r.extractor_cpu_percent||0).toFixed(1)}%</td><td>${Number(r.extractor_ram_mb||0).toFixed(0)} MB</td><td>${mb(r.memory_used_gb*1024)} / ${mb(r.memory_total_gb*1024)}</td><td>${mb(r.results_json_mb)}</td></tr>`}).join('');
document.getElementById('metadata').innerHTML=[...keys,'combined'].map(k=>{let x=d[k];if(!x.available)return `<tr><td>${x.machine}</td><td colspan="5">N/A — machine unavailable</td></tr>`;return `<tr><td>${x.machine}</td><td>${fmt(x.ats_traceable)}</td><td>${fmt(x.has_email)}</td><td>${fmt(x.company_number)}</td><td>${fmt(x.careers_page)}</td><td>${fmt(x.errors)}</td></tr>`}).join('');
document.getElementById('platform-head').innerHTML='<tr><th>Platform</th><th class="right">Combined</th>'+keys.map(k=>`<th class="right">${d[k].machine}</th>`).join('')+'</tr>';
document.getElementById('platforms').innerHTML=Object.keys(c.platform_counts||{}).map(p=>`<tr><td>${p}</td><td class="right">${fmt(c.platform_counts[p])}</td>`+keys.map(k=>`<td class="right">${fmt((d[k].platform_counts||{})[p])}</td>`).join('')+'</tr>').join('')}
async function poll(){try{const t=new URLSearchParams(location.search).get('token')||'';const r=await fetch('/api/stats'+(t?'?token='+encodeURIComponent(t):''),{cache:'no-store'});if(r.ok)render(await r.json())}catch(e){}setTimeout(poll,5000)} poll();
</script></body></html>'''


def serve(args: argparse.Namespace, dashboard: Dashboard) -> None:
    token = args.token or ""

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, format: str, *values: Any) -> None:
            return

        def authorized(self, allow_header: bool = False) -> bool:
            if not token:
                return True
            query = parse_qs(urlparse(self.path).query)
            if query.get("token", [""])[0] == token:
                return True
            return allow_header and self.headers.get("X-Dashboard-Token", "") == token

        def do_GET(self) -> None:
            if not self.authorized():
                self.send_response(403)
                self.end_headers()
                return
            if urlparse(self.path).path == "/api/stats":
                with dashboard.lock:
                    body = json.dumps(dashboard.data, ensure_ascii=False).encode("utf-8")
                self.send_response(200)
                self.send_header("Content-Type", "application/json; charset=utf-8")
            else:
                body = HTML.encode("utf-8")
                self.send_response(200)
                self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Cache-Control", "no-store")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_POST(self) -> None:
            if urlparse(self.path).path != "/api/push/m4":
                self.send_response(404)
                self.end_headers()
                return
            if not self.authorized(allow_header=True):
                self.send_response(403)
                self.end_headers()
                return
            try:
                length = int(self.headers.get("Content-Length", "0"))
                if length <= 0 or length > 1_000_000:
                    raise ValueError("invalid payload size")
                payload = json.loads(self.rfile.read(length))
                if not isinstance(payload, dict):
                    raise ValueError("payload must be a JSON object")
                dashboard.accept_push(payload)
                body = b'{"ok":true}\n'
                self.send_response(200)
            except (ValueError, TypeError, json.JSONDecodeError):
                body = b'{"ok":false}\n'
                self.send_response(400)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    ThreadingHTTPServer((args.bind, args.port), Handler).serve_forever()


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--bind", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8787)
    parser.add_argument("--refresh", type=int, default=5)
    parser.add_argument("--m1-state", required=True, type=Path)
    parser.add_argument("--m4-host", default="webpro@192.168.1.203")
    parser.add_argument("--m4-state", required=True)
    parser.add_argument("--crawler-config", type=Path, default=Path(__file__).with_name("crawler-servers.json"))
    parser.add_argument("--crawler-host", default="gcloud")
    parser.add_argument("--crawler-input", default="~/crawler/data/m1-input-last-100k.txt")
    parser.add_argument("--crawler-results", default="~/crawler/data/company-extractor-last-100k.jsonl")
    parser.add_argument("--crawler-log", default="~/crawler/logs/company-extractor-last-100k.log")
    parser.add_argument("--crawler-stats", default="~/crawler/data/company-extractor-last-100k.stats.json")
    parser.add_argument("--crawler-mode", choices=("auto", "disabled"), default="auto")
    parser.add_argument("--crawler2-host", default="c3-highcpu-8")
    parser.add_argument("--crawler2-input", default="~/crawler/data/m1-input-last-100k.txt")
    parser.add_argument("--crawler2-results", default="~/crawler/data/company-extractor-last-100k.jsonl")
    parser.add_argument("--crawler2-log", default="~/crawler/logs/company-extractor-last-100k.log")
    parser.add_argument("--crawler2-stats", default="~/crawler/data/company-extractor-last-100k.stats.json")
    parser.add_argument("--crawler2-mode", choices=("auto", "disabled"), default="auto")
    parser.add_argument("--token", default="")
    parser.add_argument("--push-ttl", type=int, default=20)
    parser.add_argument("--m4-mode", choices=("auto", "disabled"), default="auto")
    args = parser.parse_args()
    dashboard = Dashboard(args)
    threading.Thread(target=dashboard.loop, daemon=True).start()
    serve(args, dashboard)


if __name__ == "__main__":
    main()
