package main

import (
	"testing"
	"time"
)

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

func TestLimitedCompanyEvidenceRanksRepeatedNames(t *testing.T) {
	text := `better!” Elliot Cargill Charlton Baker Ltd
	 time.” Paul Zietsman Burton Beavon LTD
	 is operated by Bright SG Ltd
	 or licensed to Bright SG Ltd
	 date and correct, Bright SG Ltd
	 to your locality. Bright SG Ltd
	 with their operators. Bright SG Ltd
	 content on it. Bright SG Ltd
	 July 2024 © Bright SG Ltd
	 applicable law, including but not limited`
	evidence := limitedCompanyEvidenceFromText(text)
	if len(evidence.Names) == 0 || evidence.Names[0] != "Bright SG Ltd" {
		t.Fatalf("limited company ranking = %#v, want Bright SG Ltd first", evidence.Names)
	}
	if evidence.Counts[0]["count"] != 7 {
		t.Fatalf("Bright SG Ltd count = %#v, want 7", evidence.Counts[0]["count"])
	}
	for _, name := range evidence.Names {
		if name == "including but not limited" {
			t.Fatal("boilerplate phrase was treated as a company name")
		}
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
	if retryableFetch(FetchResult{Status: 429}) {
		t.Fatal("HTTP status responses should not be retried in the same pass")
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
	if _, ok := httpFallbackURL("https://example.co.uk", FetchResult{Err: temporaryTestError("context deadline exceeded")}); ok {
		t.Fatal("timeouts should not trigger a full HTTP fallback attempt")
	}
}

func TestActiveHTTPStatus(t *testing.T) {
	for _, status := range []int{200, 201, 204, 301, 302, 307, 401, 403} {
		if !activeHTTPStatus(status) {
			t.Fatalf("%d should be active", status)
		}
	}
	for _, status := range []int{0, 400, 404, 429, 500, 502, 503, 504} {
		if activeHTTPStatus(status) {
			t.Fatalf("%d should not be active", status)
		}
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
		"https://www.bamboohr.com/careers",
		"https://trends.pinpointhq.com/",
		"https://api.recruitee.com/",
		"https://tt.teamtailor.com/",
		"https://careers.microsoft.com/",
	} {
		if got := atsMatch(input); got != nil {
			t.Fatalf("atsMatch(%q) = %#v, want nil", input, got)
		}
	}
}

func TestATSMatchCanonicalizesAvatureActionPages(t *testing.T) {
	for _, test := range []struct {
		input, identifier, canonical string
	}{
		{
			input:      "https://specsavers.avature.net/clinicalquickapply",
			identifier: "specsavers", canonical: "https://specsavers.avature.net/clinicalquickapply",
		},
		{
			input:      "https://specsavers.avature.net/JoinourPartnership",
			identifier: "specsavers", canonical: "https://specsavers.avature.net/JoinourPartnership",
		},
	} {
		got := atsMatch(test.input)
		if got == nil || got["provider"] != "avature" || got["identifier"] != test.identifier || got["url"] != test.canonical {
			t.Fatalf("atsMatch(%q) = %#v", test.input, got)
		}
	}
	normal := atsMatch("https://epic.avature.net/Careers/SearchJobs")
	if normal == nil || normal["identifier"] != "epic/Careers/SearchJobs" {
		t.Fatalf("normal Avature match = %#v", normal)
	}
}

func TestATSVerificationUsesProviderEndpoints(t *testing.T) {
	candidates := []map[string]string{
		atsMatch("https://closed.bamboohr.com/careers/list"),
		atsMatch("https://jobs.ashbyhq.com/stale"),
	}
	responses := map[string]FetchResult{
		"https://closed.bamboohr.com/":                        {Status: 200, FinalURL: "https://closed.bamboohr.com/", Body: "ok"},
		"https://closed.bamboohr.com/careers/list":            {Status: 200, FinalURL: "https://www.bamboohr.com/", Body: "closed"},
		"https://jobs.ashbyhq.com/stale":                      {Status: 200, FinalURL: "https://jobs.ashbyhq.com/stale", Body: "ok"},
		"https://api.ashbyhq.com/posting-api/job-board/stale": {Status: 404, FinalURL: "https://api.ashbyhq.com/posting-api/job-board/stale", Body: `{}`},
	}
	fetch := func(raw string, _ time.Duration) FetchResult { return responses[raw] }
	verified := verifyATSCandidatesWith(candidates, 0, fetch)
	for index, candidate := range verified {
		if candidate["active"] != "false" {
			t.Fatalf("candidate %d = %#v, want inactive", index, candidate)
		}
	}
}

func TestAvatureActionPageRequiresWorkingSearchEndpoint(t *testing.T) {
	candidate := atsMatch("https://specsavers.avature.net/clinicalquickapply")
	responses := map[string]FetchResult{
		"https://specsavers.avature.net/clinicalquickapply": {
			Status: 200, FinalURL: "https://specsavers.avature.net/clinicalquickapply",
			Body: `<meta name="avature.portal.lang" content="en_GB"><meta name="avature.portal.urlPath" content="clinicalquickapply">`,
		},
		"https://specsavers.avature.net/clinicalquickapply/SearchJobs/": {
			Status: 404, FinalURL: "https://specsavers.avature.net/clinicalquickapply/SearchJobs/",
		},
	}
	fetch := func(raw string, _ time.Duration) FetchResult { return responses[raw] }
	verified := verifyATSCandidatesWith([]map[string]string{candidate}, 0, fetch)
	if verified[0]["active"] != "false" {
		t.Fatalf("candidate = %#v, want inactive", verified[0])
	}
}

type temporaryTestError string

func (e temporaryTestError) Error() string { return string(e) }
