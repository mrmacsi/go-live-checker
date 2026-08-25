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
