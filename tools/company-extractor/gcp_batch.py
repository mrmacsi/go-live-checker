#!/usr/bin/env python3
"""Launch, monitor, collect, and destroy a multi-VM Go extractor batch."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
LIVE = ROOT / "company-extractor" / "live-dashboard"
CONFIG = Path(__file__).with_name("crawler-servers.json")
ACTIVE_CONFIG = Path(__file__).with_name("crawler-servers-current.json")
PROJECT = "contentpulse-vertex-ai"
ACCOUNT = "contentpulseio@gmail.com"
DEFAULT_ZONE = "europe-west2-b"
DEFAULT_TYPE = "c3-highcpu-4"
M1_AGENT = Path.home() / "Library/LaunchAgents/com.sponsorcompanies.m1-runner.plist"


def gcloud(*args: str) -> list[str]:
    return ["gcloud", "--project", PROJECT, "--account", ACCOUNT, *args]


def run(command: list[str], *, capture: bool = False) -> str:
    completed = subprocess.run(command, check=True, text=True, capture_output=capture)
    return completed.stdout if capture else ""


def gssh(instance: str, zone: str, command: str, *, capture: bool = False) -> str:
    return run(gcloud("compute", "ssh", instance, "--zone", zone, "--quiet", "--command", command), capture=capture)


def gscp(local: Path, instance: str, remote: str, zone: str) -> None:
    run(gcloud("compute", "scp", str(local), f"{instance}:{remote}", "--zone", zone, "--quiet"))


def gssh_background(instance: str, zone: str, command: str) -> None:
    """Start a remote job without making the launcher wait on the SSH channel."""
    subprocess.Popen(
        gcloud("compute", "ssh", instance, "--zone", zone, "--quiet", "--command", command),
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        start_new_session=True,
    )


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for block in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def count_lines(path: Path) -> int:
    """Count newline-delimited records without loading the file into memory."""
    with path.open(encoding="utf-8", errors="replace") as handle:
        return sum(1 for _ in handle)


def split_file(source: Path, parts_dir: Path, expected_sizes: list[int]) -> list[Path]:
    """Stream one input file into balanced shard files."""
    parts_dir.mkdir(parents=True, exist_ok=True)
    parts: list[Path] = []
    with source.open("r", encoding="utf-8", errors="replace") as handle:
        for index, expected in enumerate(expected_sizes):
            part = parts_dir / f"part-{index:02d}"
            with part.open("w", encoding="utf-8") as output:
                for _ in range(expected):
                    line = handle.readline()
                    if not line:
                        raise RuntimeError("input ended before all split parts were written")
                    output.write(line)
            parts.append(part)
        if handle.readline():
            raise RuntimeError("input contains more records than the requested split total")
    return parts


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("launch", "collect-destroy"))
    parser.add_argument("--queue", type=Path, help="existing queue; with --tail, the tail is moved to the batch")
    parser.add_argument("--input", type=Path, help="complete domain file to split; the source is never modified")
    parser.add_argument("--batch-id", required=True)
    parser.add_argument("--tail", type=int, help="number of domains to take from --queue (default: 500000)")
    parser.add_argument("--servers", type=int, default=4)
    parser.add_argument("--machine-type", default=DEFAULT_TYPE)
    parser.add_argument("--zone", default=DEFAULT_ZONE)
    parser.add_argument(
        "--zones",
        help="comma-separated zones to distribute instances across (overrides --zone)",
    )
    parser.add_argument("--workers", type=int, default=128)
    parser.add_argument("--timeout", type=int, default=8)
    parser.add_argument("--attempts", type=int, default=2)
    parser.add_argument("--batch-size", type=int, default=2000)
    parser.add_argument("--force", action="store_true", help="collect/delete incomplete jobs")
    parser.add_argument("--no-m1-pause", action="store_true")
    parser.add_argument("--keep-queue", action="store_true", help="split the input without trimming the source file")
    parser.add_argument("--reuse-input", action="store_true", help="reuse an existing failed launch split")
    return parser.parse_args()


def ssh_config_host(instance: str, zone: str) -> str:
    dry_run = run(gcloud("compute", "ssh", instance, "--zone", zone, "--quiet", "--dry-run"), capture=True)
    identity_match = re.search(r"(?:^|\s)-i\s+(\S+)", dry_run)
    target_match = re.search(r"([A-Za-z0-9_.-]+@[0-9.]+)\s*$", dry_run.strip(), re.MULTILINE)
    if not identity_match or not target_match:
        raise RuntimeError(f"could not derive SSH settings for {instance}: {dry_run[-500:]}")
    user, address = target_match.group(1).split("@", 1)
    config_path = Path.home() / ".ssh/config"
    config_path.parent.mkdir(mode=0o700, exist_ok=True)
    existing = config_path.read_text(encoding="utf-8") if config_path.exists() else ""
    block = (
        f"\nHost {instance}\n"
        f"  HostName {address}\n"
        f"  User {user}\n"
        f"  IdentityFile {identity_match.group(1)}\n"
        f"  HostKeyAlias {instance}\n"
        "  IdentitiesOnly yes\n"
        "  StrictHostKeyChecking accept-new\n"
    )
    if re.search(rf"(?m)^Host {re.escape(instance)}$", existing):
        existing = re.sub(rf"(?ms)^Host {re.escape(instance)}$.*?(?=^Host |\Z)", block.lstrip("\n"), existing)
    else:
        existing += block
    config_path.write_text(existing, encoding="utf-8")
    return instance


def pause_m1() -> bool:
    if not M1_AGENT.exists():
        return False
    domain = f"gui/{os.getuid()}"
    check = subprocess.run(["launchctl", "print", f"{domain}/com.sponsorcompanies.m1-runner"], capture_output=True)
    if check.returncode != 0:
        return False
    run(["launchctl", "bootout", domain, str(M1_AGENT)])
    return True


def resume_m1() -> None:
    if M1_AGENT.exists():
        subprocess.run(["launchctl", "bootstrap", f"gui/{os.getuid()}", str(M1_AGENT)], check=False)


def update_dashboard(entries: list[dict], remove: set[str] | None = None) -> None:
    remove = remove or set()
    for path in (CONFIG, ACTIVE_CONFIG):
        current = json.loads(path.read_text(encoding="utf-8")) if path.exists() else []
        current = [item for item in current if item.get("key") not in remove]
        current.extend(entries)
        temporary = path.with_suffix(path.suffix + ".tmp")
        temporary.write_text(json.dumps(current, indent=2) + "\n", encoding="utf-8")
        os.replace(temporary, path)


def launch(args: argparse.Namespace) -> None:
    if args.input is not None and args.queue is not None:
        raise SystemExit("use either --input or --queue, not both")
    if not args.reuse_input and args.input is None and args.queue is None:
        raise SystemExit("--input or --queue must point to an existing file")
    if args.input is not None and not args.input.is_file():
        raise SystemExit("--input must point to an existing file")
    if args.queue is not None and not args.queue.is_file():
        raise SystemExit("--queue must point to an existing file")
    if args.input is not None and args.tail is not None:
        raise SystemExit("--tail is only valid with --queue")
    if args.input is None and args.tail is None:
        args.tail = 500_000
    if args.tail is not None and args.tail < 1:
        raise SystemExit("tail must be positive")
    if args.servers < 1:
        raise SystemExit("servers must be positive")
    if not re.fullmatch(r"[a-z][a-z0-9-]{2,50}", args.batch_id):
        raise SystemExit("batch-id must contain lowercase letters, digits, and hyphens")

    batch_dir = LIVE / "batches" / args.batch_id
    manifest_path = batch_dir / "manifest.json"
    if manifest_path.exists():
        raise SystemExit(f"batch already exists: {manifest_path}")
    if args.reuse_input:
        if not batch_dir.is_dir() or not (batch_dir / "input-all.txt").exists():
            raise SystemExit(f"reusable split not found: {batch_dir}")
    else:
        batch_dir.mkdir(parents=True, exist_ok=True)
    queue = args.queue.resolve() if args.queue is not None else None
    input_path = args.input.resolve() if args.input is not None else None
    zones = [zone.strip() for zone in (args.zones.split(",") if args.zones else [args.zone]) if zone.strip()]
    if not zones:
        raise SystemExit("at least one zone is required")
    instances = [f"{args.batch_id}-{i:02d}" for i in range(1, args.servers + 1)]
    instance_zones = [zones[index % len(zones)] for index in range(args.servers)]
    created: list[str] = []
    m1_was_loaded = False
    boot_disk_type = "hyperdisk-balanced" if args.machine_type.startswith("c4-") else "pd-balanced"
    try:
        for instance, zone in zip(instances, instance_zones):
            run(gcloud("compute", "instances", "create", instance, "--zone", zone, "--machine-type", args.machine_type, "--image-family", "debian-12", "--image-project", "debian-cloud", "--boot-disk-size", "20GB", "--boot-disk-type", boot_disk_type, "--network", "default", "--quiet"))
            created.append(instance)

        go = shutil.which("go") or "/opt/homebrew/bin/go"
        if not Path(go).exists():
            raise SystemExit("Go is required locally; install it with: brew install go")
        binary = batch_dir / "company-extractor-linux-amd64"
        env = dict(os.environ, GOOS="linux", GOARCH="amd64", CGO_ENABLED="0")
        subprocess.run(
            [go, "build", "-o", str(binary), "./tools/company-extractor"],
            cwd=ROOT,
            env=env,
            check=True,
        )

        if not args.no_m1_pause and not args.reuse_input and args.input is None and not args.keep_queue:
            m1_was_loaded = pause_m1()
        all_input = batch_dir / "input-all.txt"
        if not args.reuse_input:
            if input_path is not None:
                # Full-file mode is non-destructive: retain the source and
                # keep an auditable copy beside the split parts.
                shutil.copyfile(input_path, all_input)
                args.tail = count_lines(all_input)
            else:
                total = count_lines(queue)
                if total < args.tail:
                    raise SystemExit(f"queue has {total} lines; need {args.tail}")
                if args.keep_queue:
                    with all_input.open("wb") as output:
                        subprocess.run(["tail", "-n", str(args.tail), str(queue)], stdout=output, check=True)
                else:
                    keep = total - args.tail
                    with all_input.open("wb") as output:
                        subprocess.run(["tail", "-n", str(args.tail), str(queue)], stdout=output, check=True)
                    trimmed = batch_dir / "queue-trimmed.txt"
                    with trimmed.open("wb") as output:
                        subprocess.run(["head", "-n", str(keep), str(queue)], stdout=output, check=True)
                    os.replace(trimmed, queue)
        args.tail = count_lines(all_input)
        if args.tail < 1:
            raise RuntimeError("input file contains no domains")
        chunk_size, remainder = divmod(args.tail, args.servers)
        expected_sizes = [chunk_size + (1 if index < remainder else 0) for index in range(args.servers)]
        if sum(expected_sizes) != args.tail:
            raise RuntimeError("input split total verification failed")
        if not args.reuse_input:
            split_file(all_input, batch_dir / "parts", expected_sizes)
        parts = sorted((batch_dir / "parts").glob("part-*"))
        if len(parts) != args.servers or any(count_lines(part) != expected for part, expected in zip(parts, expected_sizes)):
            raise RuntimeError("chunk verification failed")

        entries = []
        server_info = []
        for index, (instance, zone, part) in enumerate(zip(instances, instance_zones, parts), 1):
            for _ in range(30):
                try:
                    gssh(instance, zone, "printf ready")
                    break
                except subprocess.CalledProcessError:
                    time.sleep(5)
            else:
                raise RuntimeError(f"SSH did not become ready: {instance}")
            gssh(instance, zone, "mkdir -p $HOME/crawler/bin $HOME/crawler/data $HOME/crawler/logs && sudo mkdir -p /etc/systemd/resolved.conf.d && printf '%s\\n' '[Resolve]' 'DNS=1.1.1.1 1.0.0.1 169.254.169.254' 'FallbackDNS=1.1.1.1 1.0.0.1' 'Domains=~local' | sudo tee /etc/systemd/resolved.conf.d/99-codex-cloudflare.conf >/dev/null && sudo systemctl restart systemd-resolved || true")
            job = f"{args.batch_id}-{index:02d}"
            remote_home = gssh(instance, zone, 'printf %s "$HOME"', capture=True).strip()
            remote_root = f"{remote_home}/crawler"
            gscp(binary, instance, f"{remote_root}/bin/company-extractor", zone)
            gscp(ROOT / "tools/company-extractor/batch_runner.py", instance, f"{remote_root}/bin/batch_runner.py", zone)
            gscp(part, instance, f"{remote_root}/data/{job}.input.txt", zone)
            gssh_background(instance, zone, f"chmod 755 {remote_root}/bin/company-extractor && nohup python3 {remote_root}/bin/batch_runner.py --input {remote_root}/data/{job}.input.txt --output {remote_root}/data/{job}.jsonl --state {remote_root}/data/{job}.state.json --binary {remote_root}/bin/company-extractor --machine {instance} --workers {args.workers} --timeout {args.timeout} --attempts {args.attempts} --batch-size {args.batch_size} > {remote_root}/logs/{job}.log 2>&1 < /dev/null &")
            ssh_target = ssh_config_host(instance, zone)
            server_info.append({"instance": instance, "job": job, "zone": zone, "ssh_target": ssh_target, "expected_total": expected})
            entries.append({"key": job, "label": f"{instance} · {args.batch_id}", "mode": "auto", "host": ssh_target, "input": f"~/crawler/data/{job}.input.txt", "results": f"~/crawler/data/{job}.jsonl", "log": f"~/crawler/logs/{job}.log", "stats": f"~/crawler/data/{job}.state.json"})
        manifest = {"batch_id": args.batch_id, "zones": zones, "project": PROJECT, "account": ACCOUNT, "machine_type": args.machine_type, "tail": args.tail, "total": args.tail, "input_mode": "full-file" if input_path is not None else "queue-tail", "source_file": str(input_path or queue) if input_path or queue else None, "servers": server_info}
        manifest_path.write_text(json.dumps(manifest, indent=2) + "\n")
        update_dashboard(entries)
        if m1_was_loaded:
            resume_m1()
            m1_was_loaded = False
        print(f"launched {args.batch_id}; manifest={manifest_path}")
    finally:
        if m1_was_loaded:
            resume_m1()
        if created and not manifest_path.exists():
            for instance, zone in zip(created, instance_zones):
                run(gcloud("compute", "instances", "delete", instance, "--zone", zone, "--quiet"), capture=True)


def collect_destroy(args: argparse.Namespace) -> None:
    batch_dir = LIVE / "batches" / args.batch_id
    manifest_path = batch_dir / "manifest.json"
    if not manifest_path.exists():
        raise SystemExit(f"manifest not found: {manifest_path}")
    manifest = json.loads(manifest_path.read_text())
    complete = True
    states = []
    total = int(manifest.get("total", manifest["tail"]))
    chunk_size, remainder = divmod(total, len(manifest["servers"]))
    if all("expected_total" in server for server in manifest["servers"]):
        expected_sizes = [int(server["expected_total"]) for server in manifest["servers"]]
    else:
        expected_sizes = [chunk_size + (1 if index < remainder else 0) for index in range(len(manifest["servers"]))]
    for index, server in enumerate(manifest["servers"]):
        job = server["job"]
        try:
            command = f"python3 -c 'import json, os; print(json.dumps(json.load(open(os.path.expanduser(\"~/crawler/data/{job}.state.json\")))))'"
            raw = gssh(server["instance"], server["zone"], command, capture=True)
            state = json.loads(raw.strip().splitlines()[-1])
        except (subprocess.CalledProcessError, json.JSONDecodeError, IndexError):
            state = {"status": "unavailable"}
        states.append((server, state))
        print(server["instance"], state.get("status"), state.get("processed"), state.get("total"))
        if state.get("status") != "complete" or state.get("processed") != expected_sizes[index]:
            complete = False
    if not complete and not args.force:
        raise SystemExit("incomplete chunk; refusing collect/delete (use --force to override)")

    for server, _state in states:
        destination = batch_dir / f"{server['job']}.jsonl"
        partial = Path(tempfile.mktemp(prefix=destination.name + ".partial.", dir=batch_dir))
        run(gcloud("compute", "scp", f"{server['instance']}:~/crawler/data/{server['job']}.jsonl", str(partial), "--zone", server["zone"], "--quiet"))
        remote_hash = gssh(server["instance"], server["zone"], f"sha256sum $HOME/crawler/data/{server['job']}.jsonl", capture=True).split()[0]
        local_hash = sha256(partial)
        if remote_hash != local_hash:
            raise RuntimeError(f"checksum mismatch for {server['job']}")
        os.replace(partial, destination)
        print(f"collected {destination} sha256={local_hash}")

    update_dashboard([], {server["job"] for server in manifest["servers"]})
    for server in manifest["servers"]:
        run(gcloud("compute", "instances", "delete", server["instance"], "--zone", server["zone"], "--quiet"))
    print(f"collected and deleted {args.batch_id}")


def main() -> None:
    args = parse_args()
    if args.action == "launch":
        launch(args)
    else:
        collect_destroy(args)


if __name__ == "__main__":
    main()
