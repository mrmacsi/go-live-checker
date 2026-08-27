# Remote Go company extractor jobs

Use `deploy_remote_runner.sh` to build the current extractor for Linux amd64,
copy a newline-delimited domain list to an SSH host, and start a resumable
runner. The runner appends one JSON object per line and writes a state file
after each bounded batch.

```bash
tools/company-extractor/deploy_remote_runner.sh \
  --host c3-highcpu-8 \
  --input company-extractor/live-dashboard/m1-input-last-100k-c3-highcpu-8.txt \
  --job c3-m1-tail-100k \
  --machine c3-highcpu-8 \
  --workers 128 \
  --timeout 8 \
  --attempts 2 \
  --batch-size 2000
```

Rerun the same command to redeploy the binary and resume from the existing
JSONL output. Use `--no-start` when only preparing a host. Jobs are isolated
by `--job`, so separate machines can process separate queue files without
sharing or overwriting results.

The dashboard accepts the old crawler through `--crawler-*` arguments and an
additional host through `--crawler2-*` arguments. The current C3 defaults are
`c3-highcpu-8` and the standard `~/crawler/data` / `~/crawler/logs` paths.

For the complete queue operation—pause M1, move a tail, deploy/start the job,
and register it in the dashboard—use:

```bash
tools/company-extractor/dispatch_job.sh \
  --queue company-extractor/live-dashboard/m1-input.txt \
  --host c3-highcpu-8 \
  --job c3-m1-tail-2-100k \
  --label 'C3 highcpu-8 · M1 tail 2' \
  --tail 100000
```

The script uses 128 workers, an 8-second timeout, 2 attempts, and 2,000-domain
batches by default. It pauses and resumes the M1 LaunchAgent around the queue
change, uses a lock plus atomic replacements, and refuses to overwrite an
existing extracted input. If deployment fails after the queue move, rerun
with `--reuse-input` to deploy the saved input without moving another tail.

## GCP multi-server batches

`gcp_batch.py` splits a queue tail into equal files, creates one GCE VM per
chunk, deploys the current Linux Go binary, starts resumable runners, and adds
each job to the dashboard. The current quota-safe C3 choice is four
`c3-highcpu-4` VMs in `europe-west2-b` (four `c3-highcpu-8` VMs would exceed
the project C3 CPU quota).

```bash
python3 tools/company-extractor/gcp_batch.py launch \
  --queue company-extractor/live-dashboard/m1-input.txt \
  --batch-id m1-tail-500k-20260825 \
  --tail 500000 \
  --servers 4 \
  --machine-type c3-highcpu-4 \
  --zone europe-west2-b \
  --workers 128 \
  --timeout 8 \
  --attempts 2 \
  --batch-size 2000
```

When every job reports `complete`, collect and delete the VMs with:

```bash
python3 tools/company-extractor/gcp_batch.py collect-destroy \
  --batch-id m1-tail-500k-20260825
```

The collect step verifies each downloaded JSONL with SHA-256 before deleting
the corresponding VM. It refuses to delete incomplete jobs unless
`--force` is explicitly supplied. A failed launch can reuse its already-made
split with `--reuse-input`.

### Launch a complete domain file

For a new file such as a 5-million-domain queue, use `--input` and set the
number of servers explicitly. The file is counted once, split into balanced
newline-preserving parts, copied to the VMs, and started automatically. The
source file is not changed. Dashboard entries and a manifest are written for
each shard:

```bash
python3 tools/company-extractor/gcp_batch.py launch \
  --input /absolute/path/domains.txt \
  --batch-id ct-nominet-5m-20260827 \
  --servers 10 \
  --machine-type c3-highcpu-4 \
  --zone europe-west2-b,europe-west1-b \
  --workers 128 \
  --timeout 8 \
  --attempts 2 \
  --batch-size 2000
```

If the file contains 5,000,000 domains and `--servers 10`, each server gets
500,000 domains. If the total is not evenly divisible, the first shards get
one extra line. The local manifest and split files are stored under
`company-extractor/live-dashboard/batches/<batch-id>/`.

The original queue-tail behavior remains available with `--queue --tail`.
`--zone` accepts one zone or a comma-separated list. The launcher places the
first eight instances in the first zone, the next eight in the second, and so
on. It refuses to launch if more than eight instances are requested per
supplied zone. `--zones` is retained as an equivalent explicit alias for the
same comma-separated list.
Use a unique `--batch-id`; the launcher refuses to overwrite an existing
manifest. To collect verified JSONL results and delete the VMs after all
shards finish:

```bash
python3 tools/company-extractor/gcp_batch.py collect-destroy \
  --batch-id ct-nominet-5m-20260827
```
