package main

import "testing"

func TestDetectHosting(t *testing.T) {
	tests := []struct {
		name, body, finalURL string
		want                 bool
	}{
		{"normal website", "<html><title>Example</title><p>Services</p></html>", "https://example.co.uk/", false},
		{"broker redirect host", "<html></html>", "https://www.sedo.com/lander/example.co.uk", true},
		{"parking marker", "This domain is parked. Buy this domain", "https://example.co.uk/", true},
		{"javascript lander", `<script>location.href='/lander/example.co.uk'</script>`, "https://example.co.uk/", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := detectHosting(test.body, test.finalURL); got != test.want {
				t.Fatalf("detectHosting() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestDetectWebsitePlatforms(t *testing.T) {
	tests := []struct {
		name, body, finalURL, primary string
		platforms                     []string
	}{
		{
			name:      "wordpress",
			body:      `<meta name="generator" content="WordPress"><link href="/wp-content/themes/example/style.css">`,
			finalURL:  "https://example.co.uk/",
			primary:   "wordpress",
			platforms: []string{"wordpress"},
		},
		{
			name:      "shopify",
			body:      `<script src="https://cdn.shopify.com/shop.js"></script><div class="shopify-section">`,
			finalURL:  "https://shop.example.co.uk/",
			primary:   "shopify",
			platforms: []string{"shopify"},
		},
		{
			name:      "wix",
			body:      `<script src="https://static.wixstatic.com/site.js"></script>`,
			finalURL:  "https://example.co.uk/",
			primary:   "wix",
			platforms: []string{"wix"},
		},
		{
			name:      "unknown",
			body:      `<html><title>Example</title><p>Services</p></html>`,
			finalURL:  "https://example.co.uk/",
			primary:   "",
			platforms: []string{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			primary, platforms := detectWebsitePlatforms(test.body)
			if primary != test.primary {
				t.Fatalf("detectWebsitePlatforms() primary = %q, want %q", primary, test.primary)
			}
			if len(platforms) != len(test.platforms) {
				t.Fatalf("detectWebsitePlatforms() platforms = %#v, want %#v", platforms, test.platforms)
			}
			for index := range platforms {
				if platforms[index] != test.platforms[index] {
					t.Fatalf("detectWebsitePlatforms() platforms = %#v, want %#v", platforms, test.platforms)
				}
			}
		})
	}
}

func TestRetryableFetch(t *testing.T) {
	if !retryableFetch(FetchResult{Status: 429}) {
		t.Fatal("429 should be retried")
	}
	if !retryableFetch(FetchResult{Err: temporaryTestError("connection reset by peer")}) {
		t.Fatal("connection reset should be retried")
	}
	if retryableFetch(FetchResult{Status: 404}) {
		t.Fatal("404 should not be retried")
	}
}

func TestHTTPFallbackURL(t *testing.T) {
	url, ok := httpFallbackURL("https://example.co.uk:443/path", FetchResult{Err: temporaryTestError("tls: internal error")})
	if !ok || url != "http://example.co.uk/path" {
		t.Fatalf("httpFallbackURL() = %q, %v", url, ok)
	}
	if _, ok := httpFallbackURL("https://example.co.uk", FetchResult{Err: temporaryTestError("no such host")}); ok {
		t.Fatal("DNS failures should not trigger HTTP fallback")
	}
}

func TestATSMatchSupportsEightfoldAndRejectsFalseBoards(t *testing.T) {
	tests := []struct {
		name, input, provider, identifier, canonical string
	}{
		{
			name:     "Microsoft Eightfold",
			input:    "https://apply.careers.microsoft.com/careers",
			provider: "eightfold", identifier: "careers",
			canonical: "https://apply.careers.microsoft.com/careers",
		},
		{
			name:     "Netflix Eightfold",
			input:    "https://explore.jobs.netflix.net/careers",
			provider: "eightfold", identifier: "careers",
			canonical: "https://explore.jobs.netflix.net/careers",
		},
		{
			name:     "Teamtailor board",
			input:    "https://acme.teamtailor.com/jobs",
			provider: "teamtailor", identifier: "acme",
			canonical: "https://acme.teamtailor.com",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := atsMatch(test.input)
			if got == nil || got["provider"] != test.provider || got["identifier"] != test.identifier || got["url"] != test.canonical {
				t.Fatalf("atsMatch(%q) = %#v", test.input, got)
			}
		})
	}
	for _, input := range []string{
		"https://www.greenhouse.io/de",
		"https://s101.recruiting.eu.greenhouse.io/ai_opt_out_request/job_post/4825124101/ai_opt_out",
		"https://tt.teamtailor.com/",
		"https://careers.microsoft.com/",
	} {
		if got := atsMatch(input); got != nil {
			t.Fatalf("atsMatch(%q) = %#v, want nil", input, got)
		}
	}
}

type temporaryTestError string

func (e temporaryTestError) Error() string { return string(e) }
