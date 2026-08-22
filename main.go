package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const scannerUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.6 Safari/605.1.15"

type result struct {
	Domain        string   `json:"domain"`
	DNSActive     bool     `json:"dns_active"`
	HTTPActive    bool     `json:"http_active"`
	HTTPStatus    *int     `json:"http_status"`
	FinalURL      *string  `json:"final_url"`
	RedirectCount int      `json:"redirect_count"`
	RedirectChain []string `json:"redirect_chain,omitempty"`
	CheckedAt     string   `json:"checked_at"`
	ResponseMS    float64  `json:"response_ms"`
	Error         *string  `json:"error"`
	Attempts      int      `json:"attempts"`
}

type summary struct {
	Tested          int            `json:"tested"`
	DNSActive       int            `json:"dns_active"`
	HTTPActive      int            `json:"http_active"`
	Inactive        int            `json:"inactive"`
	AverageResponse float64        `json:"average_response_ms"`
	StatusCounts    map[string]int `json:"status_counts"`
	responseTotal   float64        `json:"-"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "split":
		err = runSplit(os.Args[2:])
	case "scan":
		err = runScan(os.Args[2:])
	case "merge":
		err = runMerge(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		usage()
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `go-live-checker

Commands:
  split  Divide one domain list into deterministic parts
  scan   Check DNS and HTTP concurrently, writing resumable JSONL results
  merge  Combine result files into active/inactive domain lists

Examples:
  go-live-checker split --input uk-domains.txt --parts 2 --output-dir parts
  go-live-checker scan --input parts/part-1-of-2.txt --output results-1.jsonl --workers 512 --attempts 2 --timeout 10s --resume
  go-live-checker merge --input-dir . --pattern 'results-*.jsonl' --active-output uk-active-domains.txt --inactive-output uk-inactive-domains.txt`)
}

func runSplit(args []string) error {
	flags := flag.NewFlagSet("split", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	input := flags.String("input", "", "newline-delimited domain list")
	parts := flags.Int("parts", 2, "number of output parts")
	outputDir := flags.String("output-dir", "parts", "directory for part files")
	prefix := flags.String("prefix", "part", "output filename prefix")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *input == "" {
		return errors.New("split requires --input")
	}
	if *parts < 1 {
		return errors.New("--parts must be at least 1")
	}

	total, err := countDomains(*input)
	if err != nil {
		return err
	}
	if total == 0 {
		return errors.New("input contains no domains")
	}
	if err := os.MkdirAll(*outputDir, 0o755); err != nil {
		return err
	}

	partSize := (total + *parts - 1) / *parts
	files := make([]*os.File, *parts)
	writers := make([]*bufio.Writer, *parts)
	for i := 0; i < *parts; i++ {
		path := filepath.Join(*outputDir, fmt.Sprintf("%s-%d-of-%d.txt", *prefix, i+1, *parts))
		file, createErr := os.Create(path)
		if createErr != nil {
			return createErr
		}
		files[i] = file
		writers[i] = bufio.NewWriterSize(file, 1<<20)
	}
	defer func() {
		for i := range files {
			writers[i].Flush()
			files[i].Close()
		}
	}()

	inputFile, err := os.Open(*input)
	if err != nil {
		return err
	}
	defer inputFile.Close()

	scanner := newLineScanner(inputFile)
	written := 0
	for scanner.Scan() {
		domain := strings.TrimSpace(scanner.Text())
		if domain == "" {
			continue
		}
		part := written / partSize
		if part >= *parts {
			part = *parts - 1
		}
		if _, err := writers[part].WriteString(domain + "\n"); err != nil {
			return err
		}
		written++
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	fmt.Printf("split %d domains into %d parts in %s\n", written, *parts, *outputDir)
	return nil
}

func runScan(args []string) error {
	flags := flag.NewFlagSet("scan", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	input := flags.String("input", "", "newline-delimited domain list")
	output := flags.String("output", "results.jsonl", "JSONL result file")
	workers := flags.Int("workers", 512, "concurrent workers")
	timeout := flags.Duration("timeout", 10*time.Second, "DNS/connect/read timeout")
	batchSize := flags.Int("batch-size", 1000, "flush progress after this many results")
	attempts := flags.Int("attempts", 2, "maximum DNS/HTTP attempts per domain")
	resume := flags.Bool("resume", false, "append results and skip domains already present in the output")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *input == "" {
		return errors.New("scan requires --input")
	}
	if *workers < 1 || *timeout <= 0 || *batchSize < 1 || *attempts < 1 {
		return errors.New("workers, timeout, batch-size, and attempts must be positive")
	}

	done := map[string]struct{}{}
	if *resume {
		var err error
		done, err = loadCompletedDomains(*output)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "resume: skipping %d completed domains\n", len(done))
	}

	inputFile, err := os.Open(*input)
	if err != nil {
		return err
	}
	defer inputFile.Close()
	mode := os.O_CREATE | os.O_WRONLY
	if *resume {
		mode |= os.O_APPEND
	} else {
		mode |= os.O_TRUNC
	}
	outputFile, err := os.OpenFile(*output, mode, 0o644)
	if err != nil {
		return err
	}
	defer outputFile.Close()

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          *workers * 2,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   *timeout,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	client := &http.Client{Transport: transport, Timeout: *timeout}
	jobs := make(chan string, *workers*2)
	results := make(chan result, *workers*2)
	var workersGroup sync.WaitGroup
	for i := 0; i < *workers; i++ {
		workersGroup.Add(1)
		go func() {
			defer workersGroup.Done()
			for domain := range jobs {
				results <- scanDomain(domain, client, *timeout, *attempts)
			}
		}()
	}
	go func() {
		workersGroup.Wait()
		close(results)
	}()

	var writerGroup sync.WaitGroup
	writerGroup.Add(1)
	go func() {
		defer writerGroup.Done()
		writer := bufio.NewWriterSize(outputFile, 1<<20)
		encoder := json.NewEncoder(writer)
		count := 0
		for item := range results {
			if err := encoder.Encode(item); err != nil {
				fmt.Fprintln(os.Stderr, "result write error:", err)
				continue
			}
			count++
			if count%*batchSize == 0 {
				writer.Flush()
				fmt.Fprintf(os.Stderr, "tested %d\n", count)
			}
		}
		writer.Flush()
	}()

	scanner := newLineScanner(inputFile)
	queued := 0
	for scanner.Scan() {
		domain := strings.TrimSpace(scanner.Text())
		if domain == "" {
			continue
		}
		if _, exists := done[domain]; exists {
			continue
		}
		jobs <- domain
		queued++
	}
	close(jobs)
	if err := scanner.Err(); err != nil {
		return err
	}
	writerGroup.Wait()
	fmt.Fprintf(os.Stderr, "queued %d domains\n", queued)
	return nil
}

func scanDomain(domain string, client *http.Client, timeout time.Duration, attempts int) result {
	started := time.Now()
	var last result
	for attempt := 1; attempt <= attempts; attempt++ {
		last = scanDomainAttempt(domain, client, timeout)
		last.Attempts = attempt
		if !shouldRetry(last) || attempt == attempts {
			last.ResponseMS = elapsedMS(started)
			return last
		}
		time.Sleep(time.Duration(attempt) * 250 * time.Millisecond)
	}
	last.ResponseMS = elapsedMS(started)
	return last
}

func scanDomainAttempt(domain string, client *http.Client, timeout time.Duration) result {
	item := result{Domain: domain, CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	_, dnsErr := net.DefaultResolver.LookupHost(ctx, domain)
	cancel()
	if dnsErr != nil {
		item.Error = stringPtr(dnsErr.Error())
		return item
	}
	item.DNSActive = true

	for _, scheme := range []string{"https", "http"} {
		redirectCount := 0
		redirectChain := []string{scheme + "://" + domain}
		requestCtx, requestCancel := context.WithTimeout(context.Background(), timeout)
		req, requestErr := http.NewRequestWithContext(requestCtx, http.MethodGet, redirectChain[0], nil)
		if requestErr != nil {
			requestCancel()
			continue
		}
		req.Header.Set("User-Agent", scannerUserAgent)
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Range", "bytes=0-0")
		clientForRequest := *client
		clientForRequest.CheckRedirect = func(next *http.Request, via []*http.Request) error {
			redirectCount++
			redirectChain = append(redirectChain, next.URL.String())
			if redirectCount >= 10 {
				return http.ErrUseLastResponse
			}
			return nil
		}
		resp, requestErr := clientForRequest.Do(req)
		requestCancel()
		if resp == nil {
			if requestErr != nil {
				item.Error = stringPtr(requestErr.Error())
			}
			continue
		}

		status := resp.StatusCode
		item.HTTPStatus = &status
		finalURL := resp.Request.URL.String()
		item.FinalURL = &finalURL
		item.RedirectCount = redirectCount
		item.RedirectChain = redirectChain
		// Liveness checks do not need page content. Close the body immediately
		// after headers are received so HTML is not downloaded or parsed.
		resp.Body.Close()
		item.HTTPActive = status == http.StatusOK || (status >= 300 && status < 400) || status == http.StatusUnauthorized || status == http.StatusForbidden
		if requestErr != nil && requestErr != http.ErrUseLastResponse {
			item.Error = stringPtr(requestErr.Error())
		}
		return item
	}

	return item
}

func shouldRetry(item result) bool {
	if !item.DNSActive || item.HTTPStatus == nil {
		return true
	}
	status := *item.HTTPStatus
	return status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests || status >= 500
}

func runMerge(args []string) error {
	flags := flag.NewFlagSet("merge", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	inputDir := flags.String("input-dir", ".", "directory containing JSONL result files")
	pattern := flags.String("pattern", "results-*.jsonl", "glob pattern inside input-dir")
	activeOutput := flags.String("active-output", "uk-active-domains.txt", "active domain output")
	inactiveOutput := flags.String("inactive-output", "uk-inactive-domains.txt", "inactive domain output")
	summaryOutput := flags.String("summary-output", "scan-summary.json", "summary JSON output")
	if err := flags.Parse(args); err != nil {
		return err
	}
	paths, err := filepath.Glob(filepath.Join(*inputDir, *pattern))
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("no result files matched %s", filepath.Join(*inputDir, *pattern))
	}
	sort.Strings(paths)

	activeTemp := *activeOutput + ".part"
	inactiveTemp := *inactiveOutput + ".part"
	activeFile, err := os.Create(activeTemp)
	if err != nil {
		return err
	}
	inactiveFile, err := os.Create(inactiveTemp)
	if err != nil {
		activeFile.Close()
		return err
	}
	activeWriter := bufio.NewWriterSize(activeFile, 1<<20)
	inactiveWriter := bufio.NewWriterSize(inactiveFile, 1<<20)
	stats := summary{StatusCounts: map[string]int{}}

	for _, path := range paths {
		file, openErr := os.Open(path)
		if openErr != nil {
			return openErr
		}
		scanner := newLineScanner(file)
		for scanner.Scan() {
			var item result
			if err := json.Unmarshal(scanner.Bytes(), &item); err != nil {
				file.Close()
				return fmt.Errorf("invalid JSON in %s: %w", path, err)
			}
			stats.Tested++
			stats.ResponseMSAdd(item.ResponseMS)
			if item.DNSActive {
				stats.DNSActive++
			}
			if item.HTTPActive {
				stats.HTTPActive++
				activeWriter.WriteString(item.Domain + "\n")
			} else {
				stats.Inactive++
				inactiveWriter.WriteString(item.Domain + "\n")
			}
			if item.HTTPStatus != nil {
				stats.StatusCounts[fmt.Sprintf("%d", *item.HTTPStatus)]++
			}
		}
		if scanErr := scanner.Err(); scanErr != nil {
			file.Close()
			return scanErr
		}
		file.Close()
	}
	activeWriter.Flush()
	inactiveWriter.Flush()
	activeFile.Close()
	inactiveFile.Close()

	if err := sortUnique(activeTemp, *activeOutput); err != nil {
		return err
	}
	if err := sortUnique(inactiveTemp, *inactiveOutput); err != nil {
		return err
	}
	os.Remove(activeTemp)
	os.Remove(inactiveTemp)
	if stats.Tested > 0 {
		stats.AverageResponse = stats.responseTotal / float64(stats.Tested)
	}
	stats.responseTotal = 0
	data, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(*summaryOutput, append(data, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("merged %d result files: tested=%d active=%d inactive=%d\n", len(paths), stats.Tested, stats.HTTPActive, stats.Inactive)
	return nil
}

func (s *summary) ResponseMSAdd(value float64) { s.responseTotal += value }

func countDomains(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	scanner := newLineScanner(file)
	count := 0
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			count++
		}
	}
	return count, scanner.Err()
}

func loadCompletedDomains(path string) (map[string]struct{}, error) {
	done := map[string]struct{}{}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return done, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := newLineScanner(file)
	for scanner.Scan() {
		var item result
		if err := json.Unmarshal(scanner.Bytes(), &item); err != nil {
			return nil, fmt.Errorf("invalid JSON in resume file: %w", err)
		}
		done[item.Domain] = struct{}{}
	}
	return done, scanner.Err()
}

func sortUnique(input, output string) error {
	if err := exec.Command("sort", "-u", input, "-o", output).Run(); err != nil {
		return fmt.Errorf("sort -u failed: %w", err)
	}
	return nil
}

func newLineScanner(reader io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	return scanner
}

func elapsedMS(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000
}

func stringPtr(value string) *string { return &value }
