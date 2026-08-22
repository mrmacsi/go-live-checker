# go-live-checker

Standalone concurrent DNS/HTTP liveness checker for large domain lists. It is
designed to split one list across multiple laptops, run scans independently,
resume interrupted scans, and merge the results into active/inactive files.

## Build

```bash
go build -o go-live-checker .
```

Go 1.24+ is recommended. The program uses only the Go standard library.

## Split one list across two laptops

Run once on the first laptop:

```bash
./go-live-checker split \
  --input uk-domains-commoncrawl.txt \
  --parts 2 \
  --output-dir parts
```

This creates:

```text
parts/part-1-of-2.txt
parts/part-2-of-2.txt
```

Copy one part to each laptop. The two files are deterministic and do not
overlap.

## Scan each part

Laptop 1:

```bash
./go-live-checker scan \
  --input parts/part-1-of-2.txt \
  --output results-1.jsonl \
  --workers 512 \
  --attempts 2 \
  --timeout 10s \
  --resume
```

Laptop 2 uses `part-2-of-2.txt` and `results-2.jsonl`.

The scanner:

- resolves DNS before HTTP checking
- tries HTTPS, then HTTP if HTTPS fails
- follows redirects and records the final URL and redirect chain
- records final HTTP status, without downloading or parsing HTML bodies
- retries transient DNS/HTTP failures up to two total attempts
- considers 200, 3xx, 401, and 403 active
- writes one JSON result per domain
- resumes by skipping domains already present in the result file

The default User-Agent is a macOS Safari-compatible User-Agent. It does not
impersonate Googlebot or another crawler.

## Merge both laptops

Copy both result files into one directory, then run:

```bash
./go-live-checker merge \
  --input-dir . \
  --pattern 'results-*.jsonl' \
  --active-output uk-active-domains.txt \
  --inactive-output uk-inactive-domains.txt \
  --summary-output scan-summary.json
```

The merge uses external `sort -u`, so the final domain lists are deduplicated
without holding every domain in memory.

## Tuning

Start with 512 workers and a 10-second timeout. Higher concurrency can be
faster, but DNS resolvers and remote websites may rate-limit the scanner. The
scanner sends a small range hint and closes the response immediately after
headers; it does not download or parse HTML.

## Result example

```json
{
  "domain": "example.co.uk",
  "dns_active": true,
  "http_active": true,
  "http_status": 200,
  "final_url": "https://www.example.co.uk/",
  "title": "Example",
  "redirect_count": 1,
  "redirect_chain": [
    "https://example.co.uk",
    "https://www.example.co.uk/"
  ],
  "response_ms": 412.3
}
```
