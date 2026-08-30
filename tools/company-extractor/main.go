// Standalone Go company website extractor.
//
// This is the Go implementation of the data-extraction contract exposed by
// scripts/company_extractor.py. It keeps the same CLI shape and top-level JSON
// shape, follows redirects, bounds HTML reads, and performs bounded recursive
// same-site page inspection. Network fallback/proxy behavior is deliberately
// configured outside this binary so parity runs can use the same direct
// transport on both implementations.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	xhtml "golang.org/x/net/html"
)

const (
	maxReadBytes = 2_000_000
	maxPages     = 8
)

var userAgents = []string{
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/131.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/130.0 Safari/537.36",
}

var (
	spaceRE          = regexp.MustCompile(`\s+`)
	emailRE          = regexp.MustCompile(`(?i)[a-z0-9.!#$%&'*+/=?^_{|}~-]+@[a-z0-9-]+(?:\.[a-z0-9-]+)+`)
	postcodeRE       = regexp.MustCompile(`(?i)[A-Z]{1,2}[0-9][A-Z0-9]?\s?[0-9][A-Z]{2}`)
	postcodeLabel    = regexp.MustCompile(`(?i)(?:post\s*code|postcode)[:\s]*([A-Z]{1,2}[0-9][A-Z0-9]?\s?[0-9][A-Z]{2})`)
	regNumberRE      = regexp.MustCompile(`(?i)(?:(?:company|companies house|registered|registration)\s*(?:number|no|registration|details)?\s*[:#-]?\s*)((?:[A-Z]{2}\s*)?[0-9]{6,8})\b`)
	limitedRE        = regexp.MustCompile(`(?i)\b(?:l\.?\s*t\.?\s*d\.?|limited|p\.?\s*l\.?\s*c\.?|l\.?\s*l\.?\s*p\.?|inc\.?|incorporated|corp\.?|corporation)\b`)
	jsonLDRE         = regexp.MustCompile(`(?is)<script[^>]+type=["']application/ld\+json["'][^>]*>(.*?)</script>`)
	landerRedirectRE = regexp.MustCompile(`(?i)(window\.)?location(\.href)?\s*=\s*["']/?(lander|parking|domain([-_ ]for[-_ ]sale)?)([/\s'"?]|$)`)
	landerReplaceRE  = regexp.MustCompile(`(?i)location\.replace\(\s*["']/?(lander|parking|domain)([/\s'"?]|$)`)
	hostingPathRE    = regexp.MustCompile(`(?i)(^|/)(lander|parking|parked|domain-for-sale)(/|$)`)
	dudaAssetRE      = regexp.MustCompile(`(?i)(^|["'=(\s])/(?:_dm|dmassets)(?:[/\?#]|\\u002f|$)`)
	dudaAPIRE        = regexp.MustCompile(`(?i)\b(?:window\.)?dmapi\b`)
	dudaRuntimeRE    = regexp.MustCompile(`(?i)\b(?:dmcore|dmx|dmmobile|dmroot|duda(?:-site|-runtime)?)\b`)
)

var fallbackDNSResolvers = []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53"}

func lookupWithFallback(ctx context.Context, host string) ([]string, error) {
	addresses, err := net.DefaultResolver.LookupHost(ctx, host)
	if err == nil || isDNSNotFound(err) {
		return addresses, err
	}
	for _, server := range fallbackDNSResolvers {
		dnsServer := server
		resolver := &net.Resolver{
			PreferGo: true,
			Dial: func(dialCtx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(dialCtx, network, dnsServer)
			},
		}
		addresses, fallbackErr := resolver.LookupHost(ctx, host)
		if fallbackErr == nil || isDNSNotFound(fallbackErr) {
			return addresses, fallbackErr
		}
	}
	return nil, err
}

func isDNSNotFound(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

func dialWithDNSFallback(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) != nil {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	// Preserve the fast standard path. Public DNS is consulted only when the
	// system dial fails with a transient DNS error; doing a LookupHost before
	// every connection needlessly halves throughput on large queues.
	conn, dialErr := (&net.Dialer{}).DialContext(ctx, network, address)
	if dialErr == nil {
		return conn, nil
	}
	var dnsErr *net.DNSError
	if !errors.As(dialErr, &dnsErr) || dnsErr.IsNotFound {
		return nil, dialErr
	}
	addresses, err := lookupWithFallback(ctx, host)
	if err != nil {
		return nil, dialErr
	}
	var lastErr error
	for _, resolved := range addresses {
		conn, dialErr := (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(resolved, port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no addresses for %s", host)
}

var domainBrokerHosts = map[string]bool{
	"sedo.com": true, "sedoparking.com": true, "dan.com": true, "afternic.com": true,
	"hugedomains.com": true, "ibrandmaker.com": true, "dynadot.com": true, "efty.com": true,
	"undeveloped.com": true, "namebright.com": true, "parkingcrew.net": true, "bodis.com": true,
	"cashparking.com": true, "above.com": true, "tucows.com": true, "realnames.com": true,
	"forsale.com": true, "domainstore.com": true, "godaddy.com": true, "buydomains.com": true,
	"parklogic.com": true, "voodoo.com": true, "parked.com": true, "domainmarket.com": true,
	"epik.com": true, "snapnames.com": true, "domainagents.com": true, "names.co.uk": true,
	"banggood.com":   true,
	"any-domains.uk": true,
}

var blockedGreenhouseHosts = map[string]bool{
	"greenhouse.io":            true,
	"www.greenhouse.io":        true,
	"boards-api.greenhouse.io": true,
	"api.greenhouse.io":        true,
}

var blockedGreenhouseSegments = map[string]bool{
	"ai_opt_out_request": true,
}

var atsBlockedFragments = []string{
	"careers.smartrecruiters.com/yellltd",
	"jobs.smartrecruiters.com/yellltd",
	"business.yell.com/features/yell-ads-and-jobs",
}

var atsPersonioHostRE = regexp.MustCompile(`(?i)^[a-z0-9-]+\.jobs\.personio\.[a-z.]+$`)
var atsWorkdayLocaleRE = regexp.MustCompile(`(?i)^[a-z]{2}-[a-z]{2}$`)
var atsAvatureLocaleRE = regexp.MustCompile(`(?i)^[a-z]{2}(?:_[a-z]{2})?$`)

var atsLeverBlockedSegments = map[string]bool{
	"about": true, "blog": true, "candidate-privacy": true, "careers": true,
	"contact": true, "cookies": true, "legal": true, "privacy": true,
	"privacy-policy": true, "resources": true, "security": true, "terms": true,
}

var atsWorkableBlockedSegments = map[string]bool{
	"api": true, "app": true, "docs": true, "login": true,
	"resources": true, "status": true, "support": true,
}

var atsRipplingLocales = map[string]bool{
	"en-au": true, "en-gb": true, "en-us": true, "en": true, "fr": true,
	"fr-fr": true, "de": true, "de-de": true, "es": true, "es-es": true,
	"it": true, "nl": true, "pt": true, "pt-br": true,
}

var atsNetworxSuffixes = []string{".current-vacancies.com", ".networxrecruitment.com", ".networxrecruitment.net"}
var atsNetworxBlockedHosts = map[string]bool{"www": true, "api": true, "utils": true, "static": true, "cdn": true, "images": true}
var atsPaylocityBoardRoutes = map[string]bool{"all": true, "list": true}
var atsSmartRecruitersNonBoardSegments = map[string]bool{
	"api": true, "candidate": true, "embed": true, "job-alert": true, "job-widget": true,
	"legal": true, "login": true, "resources": true, "static": true, "subscriptions": true,
}
var atsAbsURLRE = regexp.MustCompile(`(?i)https?://[^\s"'<>\\)]+`)

var blockedTeamtailorSubdomains = map[string]bool{
	"www": true, "app": true, "api": true, "login": true, "admin": true,
	"career": true, "docs": true, "support": true, "status": true,
	"trust": true, "tt": true,
}

var blockedATSSubdomains = map[string]map[string]bool{
	"bamboohr.com":        {"www": true, "app": true, "api": true, "login": true, "resources": true, "cdn": true},
	"pinpointhq.com":      {"www": true, "app": true, "apply": true, "api": true, "developers": true, "trends": true},
	"recruitee.com":       {"www": true, "app": true, "api": true, "status": true, "support": true},
	"breezy.hr":           {"www": true, "app": true, "api": true, "status": true, "support": true, "m": true},
	"careers.hibob.com":   {"www": true, "app": true},
	"livevacancies.co.uk": {"www": true, "app": true},
	"softgarden.io":       {"www": true, "app": true, "api": true, "login": true, "docs": true, "support": true, "status": true},
	"hire.trakstar.com":   {"www": true, "app": true, "api": true, "status": true, "support": true},
	"talent-soft.com":     {"www": true, "app": true, "api": true, "login": true, "support": true},
	"ttcportals.com":      {"www": true, "app": true, "api": true, "login": true, "support": true},
	"schoolrecruiter.com": {"www": true, "app": true, "api": true, "login": true, "support": true},
}

var avatureActionSegments = map[string]bool{
	"applyflowcheck": true, "applicationform": true, "clinicalquickapply": true,
	"eventlisting": true, "eventslisting": true, "feed": true, "folderdetail": true,
	"frontpage": true, "home": true, "jobdetail": true, "joinourpartnership": true, "login": true, "profile": true,
	"register": true, "searchjobs": true, "talentcommunity": true,
}

var junkEmailExact = map[string]bool{
	"email@email.com": true, "email@email.co.uk": true, "email@mail.com": true,
	"admin@admin.com": true, "john@doe.com": true, "jane@doe.com": true,
	"abc@xyz.com": true, "abc@mail.com": true, "abc@gmail.com": true,
	"xyz@xyz.com": true, "xyz@abc.com": true, "xyz@gmail.com": true,
	"null@null.null": true, "your@email.com": true, "your@email.here": true,
	"name@company.com": true, "name@company.co.uk": true, "name@domain.com": true,
	"email@domain.com": true, "test@test.com": true, "test@mail.com": true,
	"test@fb.com": true, "test@whatsapp.com": true, "info@info.info": true,
}

var junkEmailDomains = map[string]bool{
	"example.com": true, "example.org": true, "example.net": true, "sample.com": true,
	"sample.org": true, "dummy.com": true, "invalid.com": true, "invalid.name": true,
	"mailinator.com": true, "yopmail.com": true, "yopmail.fr": true,
	"guerrillamail.com": true, "guerrillamail.de": true, "guerrillamail.net": true,
	"sharklasers.com": true, "grr.la": true, "mysite.com": true,
}

var pageKeywords = map[string][]string{
	"about_page_link":                {"/about", "/about-us", "/who-we-are"},
	"contact_page_link":              {"/contact", "/contact-us", "/get-in-touch", "/enquir", "/reach"},
	"careers_page_link":              {"/career", "/jobs", "/work-with-us", "/join", "/employment", "/hiring", "/opportunit"},
	"vacancies_page_link":            {"/vacanc", "/job-openings", "/current-jobs", "/apply", "/roles"},
	"terms_and_conditions_page_link": {"/terms", "/t-and-c", "/tcs", "/conditions"},
	"privacy_policy_page_link":       {"/privacy", "/data-protection", "/cookie"},
	"legal_page_link":                {"/legal", "/company-information", "/registered-office", "/imprint"},
}

var socialDomains = map[string][]string{
	"twitter": {"twitter.com", "x.com"}, "instagram": {"instagram.com"}, "youtube": {"youtube.com"},
	"tiktok": {"tiktok.com"}, "linkedin": {"linkedin.com"}, "facebook": {"facebook.com"},
}

type Anchor struct{ Href, Text string }
type Image struct{ Href, Alt, Class, Rel, Sizes string }
type Page struct {
	Title, Description, OGImage, Text, FooterText string
	Headings                                      []string
	Anchors                                       []Anchor
	Emails                                        []map[string]string
	Phones                                        []map[string]string
	Images                                        []Image
	Structured                                    map[string]interface{}
}

type FetchResult struct {
	Status   int
	FinalURL string
	Body     string
	Err      error
	Via      string
	Attempts []map[string]interface{}
}

type FetchConfig struct {
	Proxy         *url.URL
	Browser       bool
	BrowserScript string
	Attempts      int
}

var fetchConfig FetchConfig

// A small browser pool avoids the Python implementation's serialized-browser
// bottleneck without launching one Chromium process per worker.
var browserSlots = make(chan struct{}, 4)
var httpClientOnce sync.Once
var sharedHTTPClient *http.Client
var httpSlots chan struct{}

func getHTTPClient() *http.Client {
	httpClientOnce.Do(func() {
		transport := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			DialContext:     dialWithDNSFallback,
			// Keep enough idle connections for high worker counts while retaining
			// the per-host cap that prevents a single site from being flooded.
			MaxIdleConns:        512,
			MaxIdleConnsPerHost: 32,
			MaxConnsPerHost:     32,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  false,
			// Python's urllib transport uses HTTP/1.1. Matching it avoids
			// intermittent DATA-after-END_STREAM errors from legacy hosts.
			ForceAttemptHTTP2: false,
		}
		if fetchConfig.Proxy != nil {
			transport.Proxy = http.ProxyURL(fetchConfig.Proxy)
		}
		sharedHTTPClient = &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// Preserve the last 3xx response instead of turning a redirect
				// loop into a false inactive result.
				if len(via) >= 10 {
					return http.ErrUseLastResponse
				}
				return nil
			},
		}
	})
	return sharedHTTPClient
}

func uniq(in []string, limit int) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, value := range in {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

func uniqFold(in []string, limit int) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, value := range in {
		key := strings.ToLower(value)
		if value == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

func cleanText(value string) string { return strings.TrimSpace(spaceRE.ReplaceAllString(value, " ")) }

func parseURL(raw string) *url.URL {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil
	}
	return parsed
}
func hostname(raw string) string {
	parsed := parseURL(raw)
	if parsed == nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}
func absoluteURL(raw, base string) string {
	u, b := parseURL(raw), parseURL(base)
	if u == nil || b == nil {
		return ""
	}
	return b.ResolveReference(u).String()
}
func sameHost(a, b string) bool { return hostname(a) != "" && hostname(a) == hostname(b) }
func stripDefaultPort(raw string) string {
	parsed := parseURL(raw)
	if parsed == nil {
		return raw
	}
	if (parsed.Scheme == "https" && parsed.Port() == "443") || (parsed.Scheme == "http" && parsed.Port() == "80") {
		parsed.Host = parsed.Hostname()
	}
	return parsed.String()
}

func parseHTML(raw, base string) Page {
	page := Page{Structured: structuredEmpty()}
	doc, err := xhtml.Parse(strings.NewReader(raw))
	if err != nil {
		return page
	}
	var body, footer []string
	skipDepth, footerDepth, titleDepth := 0, 0, 0
	var currentAnchor *Anchor
	var currentHeading *string
	var walk func(*xhtml.Node)
	walk = func(node *xhtml.Node) {
		if node.Type == xhtml.ElementNode {
			tag := strings.ToLower(node.Data)
			attrs := map[string]string{}
			for _, attribute := range node.Attr {
				attrs[strings.ToLower(attribute.Key)] = html.UnescapeString(attribute.Val)
			}
			if tag == "script" || tag == "style" || tag == "noscript" || tag == "svg" || tag == "template" {
				skipDepth++
				defer func() { skipDepth-- }()
				return
			}
			if tag == "footer" {
				footerDepth++
			}
			if tag == "title" {
				titleDepth++
			}
			if tag == "h1" || tag == "h2" || tag == "h3" {
				value := ""
				currentHeading = &value
			}
			if tag == "meta" {
				name, property := strings.ToLower(attrs["name"]), strings.ToLower(attrs["property"])
				content := strings.TrimSpace(attrs["content"])
				if page.Description == "" && (name == "description" || property == "og:description") {
					page.Description = content
				}
				if page.OGImage == "" && (property == "og:image" || property == "og:image:secure_url" || property == "twitter:image") {
					page.OGImage = content
				}
			}
			if tag == "link" {
				rel := strings.ToLower(attrs["rel"])
				if attrs["href"] != "" && (strings.Contains(rel, "icon") || strings.Contains(rel, "apple-touch-icon")) {
					page.Images = append(page.Images, Image{Href: strings.TrimSpace(attrs["href"]), Rel: rel, Sizes: attrs["sizes"]})
				}
			}
			if tag == "a" && attrs["href"] != "" {
				currentAnchor = &Anchor{Href: strings.TrimSpace(attrs["href"])}
			}
			if tag == "img" {
				source := attrs["src"]
				if source == "" {
					source = attrs["data-src"]
				}
				if source != "" {
					page.Images = append(page.Images, Image{Href: source, Alt: attrs["alt"], Class: attrs["class"]})
				}
			}
		}
		if node.Type == xhtml.TextNode && skipDepth == 0 {
			if titleDepth > 0 {
				page.Title += node.Data
			}
			if currentHeading != nil {
				*currentHeading += node.Data
			}
			if footerDepth > 0 {
				footer = append(footer, node.Data)
			}
			if strings.TrimSpace(node.Data) != "" {
				body = append(body, node.Data)
			}
			if currentAnchor != nil {
				currentAnchor.Text += node.Data
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
		if node.Type == xhtml.ElementNode {
			tag := strings.ToLower(node.Data)
			if tag == "title" {
				titleDepth--
			}
			if tag == "footer" {
				footerDepth--
			}
			if (tag == "h1" || tag == "h2" || tag == "h3") && currentHeading != nil {
				if value := cleanText(*currentHeading); value != "" {
					page.Headings = append(page.Headings, value)
				}
				currentHeading = nil
			}
			if tag == "a" && currentAnchor != nil {
				low := strings.ToLower(currentAnchor.Href)
				if strings.HasPrefix(low, "mailto:") {
					if value := cleanEmail(strings.SplitN(currentAnchor.Href[7:], "?", 2)[0]); value != "" {
						page.Emails = append(page.Emails, map[string]string{"email": value, "source_page": base})
					}
				} else if strings.HasPrefix(low, "tel:") {
					page.Phones = append(page.Phones, map[string]string{"number": strings.TrimSpace(currentAnchor.Href[4:]), "source_page": base})
				} else {
					page.Anchors = append(page.Anchors, *currentAnchor)
				}
				currentAnchor = nil
			}
		}
	}
	walk(doc)
	page.Title = strings.TrimSpace(page.Title)
	page.Text, page.FooterText = cleanText(strings.Join(body, " ")), cleanText(strings.Join(footer, " "))
	page.Structured = extractStructured(raw)
	return page
}

func structuredEmpty() map[string]interface{} {
	return map[string]interface{}{"names": []string{}, "legal_names": []string{}, "alternate_names": []string{}, "company_numbers": []string{}, "postcodes": []string{}, "towns": []string{}, "addresses": []string{}, "urls": []string{}, "types": []string{}}
}
func structuredAppend(result map[string]interface{}, key string, values ...string) {
	current, _ := result[key].([]string)
	result[key] = append(current, values...)
}
func structuredStrings(value interface{}) []string {
	if list, ok := value.([]string); ok {
		return list
	}
	if list, ok := value.([]interface{}); ok {
		out := []string{}
		for _, item := range list {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	}
	if text, ok := value.(string); ok {
		return []string{text}
	}
	return nil
}

func firstNonEmpty(values ...[]string) string {
	for _, list := range values {
		for _, value := range list {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	return ""
}
func structuredWalk(value interface{}, result map[string]interface{}) {
	switch item := value.(type) {
	case []interface{}:
		for _, child := range item {
			structuredWalk(child, result)
		}
	case map[string]interface{}:
		types := structuredStrings(item["@type"])
		relevant := false
		for _, typ := range types {
			typ = strings.ToLower(typ)
			if typ == "organization" || typ == "corporation" || typ == "localbusiness" || typ == "company" || typ == "ngo" {
				relevant = true
			}
			structuredAppend(result, "types", typ)
		}
		if graph, ok := item["@graph"]; ok {
			structuredWalk(graph, result)
		}
		if relevant {
			for _, key := range []string{"name", "brand"} {
				value := item[key]
				if object, ok := value.(map[string]interface{}); ok {
					value = object["name"]
				}
				if text, ok := value.(string); ok {
					structuredAppend(result, "names", strings.TrimSpace(text))
				}
			}
			if text, ok := item["legalName"].(string); ok {
				structuredAppend(result, "legal_names", strings.TrimSpace(text))
			}
			for _, key := range []string{"alternateName", "alternateNames"} {
				structuredAppend(result, "alternate_names", structuredStrings(item[key])...)
			}
			for _, address := range structuredValues(item["address"]) {
				if object, ok := address.(map[string]interface{}); ok {
					if text, ok := object["streetAddress"].(string); ok {
						structuredAppend(result, "addresses", text)
					}
					if text, ok := object["addressLocality"].(string); ok {
						structuredAppend(result, "towns", text)
					}
					if text, ok := object["postalCode"].(string); ok {
						if value := normalizePostcode(text); value != "" {
							structuredAppend(result, "postcodes", value)
						}
					}
				}
			}
			for _, key := range []string{"url", "sameAs"} {
				structuredAppend(result, "urls", structuredStrings(item[key])...)
			}
		}
		for _, child := range item {
			switch child.(type) {
			case map[string]interface{}, []interface{}:
				structuredWalk(child, result)
			}
		}
	}
}
func structuredValues(value interface{}) []interface{} {
	if value == nil {
		return nil
	}
	if list, ok := value.([]interface{}); ok {
		return list
	}
	return []interface{}{value}
}
func extractStructured(raw string) map[string]interface{} {
	result := structuredEmpty()
	for _, match := range jsonLDRE.FindAllStringSubmatch(raw, -1) {
		var value interface{}
		if json.Unmarshal([]byte(html.UnescapeString(match[1])), &value) == nil {
			structuredWalk(value, result)
		}
	}
	for key, value := range result {
		if list, ok := value.([]string); ok {
			result[key] = uniq(list, 25)
		}
	}
	return result
}

func normalizePostcode(value string) string {
	value = strings.ToUpper(strings.ReplaceAll(spaceRE.ReplaceAllString(value, ""), " ", ""))
	if regexp.MustCompile(`^[A-Z]{1,2}[0-9][A-Z0-9]?[0-9][A-Z]{2}$`).MatchString(value) {
		return value
	}
	return ""
}
func getPostcodes(text string) []string {
	out := []string{}
	for _, match := range postcodeLabel.FindAllStringSubmatch(text, -1) {
		out = append(out, normalizePostcode(match[1]))
	}
	for _, match := range postcodeRE.FindAllString(text, -1) {
		out = append(out, normalizePostcode(match))
	}
	return uniq(out, 25)
}
func getRegistrationNumbers(text string) []string {
	out := []string{}
	for _, match := range regNumberRE.FindAllStringSubmatch(text, -1) {
		value := strings.ReplaceAll(strings.ToUpper(match[1]), " ", "")
		if len(value) >= 8 && len(value) <= 10 {
			out = append(out, value)
		}
	}
	return uniq(out, 25)
}

func cleanPhone(value string) string {
	value = regexp.MustCompile(`[\s().-]`).ReplaceAllString(value, "")
	value = regexp.MustCompile(`^\+440+`).ReplaceAllString(value, "+44")
	value = regexp.MustCompile(`^00440+`).ReplaceAllString(value, "+44")
	value = regexp.MustCompile(`^0044`).ReplaceAllString(value, "+44")
	return value
}
func validPhone(value string) bool {
	value = cleanPhone(value)
	digits := regexp.MustCompile(`\D`).ReplaceAllString(value, "")
	national := ""
	if strings.HasPrefix(value, "+44") && len(digits) > 2 {
		national = digits[2:]
	} else if strings.HasPrefix(value, "0") && len(digits) > 1 {
		national = digits[1:]
	} else {
		return false
	}
	return len(national) >= 9 && len(national) <= 10 && strings.Contains("1235789", national[:1])
}
func getPhones(text string) []string {
	matches := regexp.MustCompile(`(?:\+44|0)(?:[\s]*[0-9]){9,10}`).FindAllString(text, -1)
	out := []string{}
	for _, match := range matches {
		value := cleanPhone(match)
		if validPhone(value) {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return uniq(out, 0)
}

func junkEmail(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	if lower == "" || strings.Contains(lower, "example") || junkEmailExact[lower] {
		return true
	}
	if at := strings.LastIndex(lower, "@"); at >= 0 && junkEmailDomains[lower[at+1:]] {
		return true
	}
	return false
}
func validEmail(value string) bool {
	return !junkEmail(value) && strings.Count(value, "@") == 1 && emailRE.MatchString(value) && !strings.ContainsAny(value, " \t\r\n")
}
func cleanEmail(value string) string {
	value = strings.Trim(strings.TrimSpace(html.UnescapeString(value)), " \t\r\n.,;:!?/\\)]}-")
	if validEmail(value) {
		return value
	}
	return ""
}
func getEmails(text string) []string {
	out := []string{}
	for _, match := range emailRE.FindAllString(text, -1) {
		if value := cleanEmail(match); value != "" {
			out = append(out, value)
		}
	}
	return uniqFold(out, 0)
}

func findPageLink(anchors []Anchor, keywords []string, base string) string {
	type candidate struct {
		url   string
		score int
	}
	candidates := []candidate{}
	for _, anchor := range anchors {
		value := absoluteURL(anchor.Href, base)
		parsed := parseURL(value)
		if parsed == nil {
			continue
		}
		path := strings.ToLower(strings.TrimRight(parsed.Path, "/"))
		score := 0
		matched := false
		for _, keyword := range keywords {
			keyword = strings.TrimLeft(keyword, "/")
			if strings.HasSuffix(path, "/"+keyword) || path == "/"+keyword {
				score += 10
				matched = true
			} else if strings.Contains(path, keyword) {
				score += 2
				matched = true
			}
		}
		if !matched {
			continue
		}
		score -= minInt(5, strings.Count(path, "/"))
		for _, slug := range []string{"/post/", "/blog/", "/article/", "/news/", "/category/", "/tag/", "/archive/", "/release/"} {
			if strings.Contains(path, slug) {
				score -= 100
			}
		}
		candidates = append(candidates, candidate{url: value, score: score})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return len(candidates[i].url) < len(candidates[j].url)
	})
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0].url
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func findSocial(anchors []Anchor, domains []string, base string) string {
	for _, anchor := range anchors {
		value := absoluteURL(anchor.Href, base)
		host := hostname(value)
		for _, domain := range domains {
			if host == domain || strings.HasSuffix(host, "."+domain) {
				return value
			}
		}
	}
	return ""
}
func classifyPage(value string) string {
	lower := strings.ToLower(value)
	for label, keywords := range map[string][]string{"vacancies_page": {"vacanc", "/job-opening", "/apply", "/role"}, "careers_page": {"/career", "/jobs", "/join", "/work-with-us", "/employment", "/hiring"}, "contact_page": {"/contact", "contact-us", "/get-in-touch", "/enquir", "/reach"}, "privacy_page": {"/privacy"}, "terms_and_conditions_page": {"/terms", "/t-and-c", "/tcs", "/conditions"}} {
		for _, keyword := range keywords {
			if strings.Contains(lower, keyword) {
				return label
			}
		}
	}
	return "website"
}

func browserFetch(raw string, timeout time.Duration) FetchResult {
	browserSlots <- struct{}{}
	defer func() { <-browserSlots }()
	script := fetchConfig.BrowserScript
	if script == "" {
		script = os.Getenv("COMPANY_EXTRACTOR_BROWSER_SCRIPT")
	}
	if script == "" {
		script = "/Users/macitsimsek/code/sponsor-companies/scripts/browser_fetch.mjs"
	}
	payload, _ := json.Marshal(map[string]interface{}{"url": raw, "timeout": int(timeout.Seconds()), "max_bytes": maxReadBytes, "user_agent": userAgents[0], "cookies": []interface{}{}})
	command := exec.Command("node", script)
	command.Stdin = bytes.NewReader(payload)
	output, err := command.Output()
	if err != nil {
		return FetchResult{FinalURL: raw, Err: fmt.Errorf("browser: %w", err), Via: "browser", Attempts: []map[string]interface{}{{"via": "browser", "status": nil, "error": err.Error()}}}
	}
	var decoded struct {
		OK        bool   `json:"ok"`
		Status    int    `json:"status"`
		FinalURL  string `json:"final_url"`
		HTML      string `json:"html"`
		Truncated bool   `json:"truncated"`
		Error     string `json:"error"`
		Via       string `json:"via"`
	}
	if err := json.Unmarshal(output, &decoded); err != nil {
		return FetchResult{FinalURL: raw, Err: fmt.Errorf("browser JSON: %w", err), Via: "browser"}
	}
	var fetchErr error
	if decoded.Error != "" {
		fetchErr = fmt.Errorf("%s", decoded.Error)
	}
	if decoded.FinalURL == "" {
		decoded.FinalURL = raw
	}
	return FetchResult{Status: decoded.Status, FinalURL: stripDefaultPort(decoded.FinalURL), Body: decoded.HTML, Err: fetchErr, Via: "browser", Attempts: []map[string]interface{}{{"via": "browser", "status": statusOrNil(decoded.Status), "error": errorOrNil(fetchErr)}}}
}

func fetchHTTP(raw string, timeout time.Duration, freshConnection bool) FetchResult {
	if httpSlots != nil {
		httpSlots <- struct{}{}
		defer func() { <-httpSlots }()
	}
	client := getHTTPClient()
	request, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return FetchResult{FinalURL: raw, Err: err, Via: "server"}
	}
	requestContext, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	request = request.WithContext(requestContext)
	request.Header.Set("User-Agent", userAgents[0])
	request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	request.Header.Set("Accept-Language", "en-GB,en;q=0.9")
	if freshConnection {
		request.Close = true
	}
	response, err := client.Do(request)
	if err != nil {
		result := FetchResult{FinalURL: raw, Err: err, Via: map[bool]string{true: "proxy", false: "server"}[fetchConfig.Proxy != nil]}
		result.Attempts = []map[string]interface{}{{"via": result.Via, "status": nil, "error": err.Error()}}
		if fetchConfig.Browser {
			return browserFetch(raw, timeout)
		}
		return result
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxReadBytes+1))
	if len(body) > maxReadBytes {
		body = body[:maxReadBytes]
	}
	via := "server"
	if fetchConfig.Proxy != nil {
		via = "proxy"
	}
	return FetchResult{Status: response.StatusCode, FinalURL: stripDefaultPort(response.Request.URL.String()), Body: string(body), Err: err, Via: via, Attempts: []map[string]interface{}{{"via": via, "status": statusOrNil(response.StatusCode), "error": errorOrNil(err)}}}
}

func retryableFetch(result FetchResult) bool {
	if result.Err != nil {
		message := strings.ToLower(result.Err.Error())
		for _, marker := range []string{"timeout", "deadline exceeded", "eof", "connection reset", "connection refused", "temporary", "tls handshake", "remote error: tls", "tls:", "operation timed out"} {
			if strings.Contains(message, marker) {
				return true
			}
		}
		return false
	}
	// A real HTTP response is already useful liveness evidence. Do not spend
	// another request retrying a status response in the same pass.
	return false
}

func httpFallbackURL(raw string, result FetchResult) (string, bool) {
	parsed := parseURL(raw)
	if parsed == nil || !strings.EqualFold(parsed.Scheme, "https") || result.Err == nil {
		return "", false
	}
	message := strings.ToLower(result.Err.Error())
	if strings.Contains(message, "no such host") || strings.Contains(message, "name resolution") {
		return "", false
	}
	for _, marker := range []string{"tls", "connection refused", "connection reset", "eof", "server gave http response to https client"} {
		if strings.Contains(message, marker) {
			parsed.Scheme = "http"
			if parsed.Port() == "443" {
				parsed.Host = parsed.Hostname()
			}
			return parsed.String(), true
		}
	}
	return "", false
}

func isAvatureLocale(segment string) bool {
	return atsAvatureLocaleRE.MatchString(segment)
}

func isRipplingLocale(segment string) bool {
	segment = strings.ToLower(segment)
	if atsRipplingLocales[segment] {
		return true
	}
	return regexp.MustCompile(`(?i)^[a-z]{2}-[a-z]{2}$`).MatchString(segment)
}

func atsPathSegments(raw string) []string {
	parsed := parseURL(raw)
	if parsed == nil {
		return nil
	}
	segments := []string{}
	for _, value := range strings.Split(strings.Trim(parsed.Path, "/"), "/") {
		if value != "" {
			segments = append(segments, value)
		}
	}
	return segments
}

func normalizeATSCandidate(raw string) string {
	raw = strings.TrimSpace(html.UnescapeString(raw))
	if raw == "" || strings.HasPrefix(strings.ToLower(raw), "javascript:") || strings.HasPrefix(strings.ToLower(raw), "mailto:") || strings.HasPrefix(strings.ToLower(raw), "tel:") || strings.HasPrefix(raw, "#") {
		return ""
	}
	return strings.TrimRight(raw, ".,;)'\"")
}

func atsIsBlocked(raw string) bool {
	needle := strings.ToLower(strings.TrimSpace(raw))
	if needle == "" {
		return true
	}
	for _, fragment := range atsBlockedFragments {
		if strings.Contains(needle, fragment) {
			return true
		}
	}
	return false
}

func canonicalizeATSURL(raw, provider string) string {
	parsed := parseURL(raw)
	if parsed == nil {
		return raw
	}
	host := strings.ToLower(parsed.Hostname())
	segments := atsPathSegments(raw)
	path := ""
	query := ""
	if len(segments) > 0 {
		path = "/" + strings.Join(segments, "/")
	}
	canonical := func(canonicalHost, canonicalPath, canonicalQuery string) string {
		result := &url.URL{Scheme: "https", Host: canonicalHost, Path: canonicalPath, RawQuery: canonicalQuery}
		return result.String()
	}
	switch provider {
	case "ashby":
		if len(segments) == 0 || strings.EqualFold(segments[0], "embed") || strings.EqualFold(segments[0], "api") {
			return raw
		}
		return canonical(host, "/"+segments[0], "")
	case "greenhouse":
		board := ""
		if len(segments) > 0 {
			board = segments[0]
		}
		if strings.EqualFold(board, "embed") {
			board = parseURL(raw).Query().Get("for")
		}
		if board == "" {
			return canonical(host, "", "")
		}
		return canonical(host, "/"+board, "")
	case "lever":
		if host == "api.lever.co" && len(segments) >= 3 && strings.EqualFold(segments[0], "v0") && strings.EqualFold(segments[1], "postings") {
			return canonical("jobs.lever.co", "/"+segments[2], "")
		}
		if len(segments) == 0 {
			return canonical(host, "", "")
		}
		return canonical(host, "/"+segments[0], "")
	case "eightfold":
		if path == "" {
			path = "/careers"
		}
		return canonical(host, path, "")
	case "teamtailor", "bamboohr", "pinpoint", "recruitee", "breezy", "hibob", "hireful", "personio":
		return canonical(host, "/", "")
	case "smartrecruiters":
		index := 0
		if len(segments) > 0 && strings.EqualFold(segments[0], "ni") {
			index = 1
		}
		if index >= len(segments) {
			return canonical(host, "", "")
		}
		return canonical(host, "/"+segments[index], "")
	case "workable":
		if host == "apply.workable.com" && len(segments) > 0 && !strings.EqualFold(segments[0], "j") {
			return canonical(host, "/"+segments[0], "")
		}
		return canonical(host, "/", "")
	case "jobvite":
		if len(segments) > 0 {
			return canonical(host, "/"+segments[0], "")
		}
	case "rippling":
		index := 0
		if len(segments) > 0 && isRipplingLocale(segments[0]) {
			index = 1
		}
		if index < len(segments) {
			return canonical(host, "/"+segments[index], "")
		}
		return ""
	case "workday_cxs":
		if len(segments) == 0 || strings.EqualFold(segments[0], "wday") {
			return ""
		}
		count := 1
		if atsWorkdayLocaleRE.MatchString(segments[0]) && len(segments) > 1 {
			count = 2
		}
		return canonical(host, "/"+strings.Join(segments[:count], "/"), "")
	case "nhs_jobs":
		path = parsed.Path
		query = parsed.RawQuery
		return canonical(host, path, query)
	case "avature":
		return canonical(host, path, "")
	case "ciphr_irecruit":
		return canonical(host, "/templates/CIPHR/job_list.aspx", "")
	case "employment_hero", "dayforce", "icims", "networx", "softgarden", "trakstar", "careers_page", "talent_soft", "ttcportals", "schoolrecruiter":
		preservedPath := parsed.Path
		if preservedPath == "" {
			preservedPath = "/"
		}
		return canonical(host, preservedPath, parsed.RawQuery)
	}
	if path != "/" {
		path = strings.TrimRight(path, "/")
	}
	return canonical(host, path, "")
}

func fetchPage(raw string, timeout time.Duration) FetchResult {
	if !strings.Contains(raw, "://") {
		raw = "https://" + strings.TrimLeft(raw, "/")
	}
	if fetchConfig.Browser {
		return browserFetch(raw, timeout)
	}
	attempts := fetchConfig.Attempts
	if attempts < 1 {
		attempts = 1
	}
	var result FetchResult
	attemptHistory := []map[string]interface{}{}
	for attempt := 0; attempt < attempts; attempt++ {
		attemptTimeout := timeout
		if attempt > 0 {
			// Keep the retry bounded: the first attempt gets the configured
			// timeout, while the retry is capped at four seconds.
			if attemptTimeout > 4*time.Second {
				attemptTimeout = 4 * time.Second
			}
		}
		result = fetchHTTP(raw, attemptTimeout, attempt > 0)
		attemptHistory = append(attemptHistory, result.Attempts...)
		if !retryableFetch(result) || attempt+1 == attempts {
			break
		}
		backoff := time.Duration(attempt+1) * 150 * time.Millisecond
		if backoff >= timeout {
			break
		}
		time.Sleep(backoff)
	}
	result.Attempts = attemptHistory
	if fallbackURL, ok := httpFallbackURL(raw, result); ok {
		fallbackTimeout := timeout
		if fallbackTimeout > 3*time.Second {
			fallbackTimeout = 3 * time.Second
		}
		fallback := fetchHTTP(fallbackURL, fallbackTimeout, true)
		attemptHistory = append(attemptHistory, fallback.Attempts...)
		if fallback.Status != 0 || fallback.Err == nil {
			result = fallback
		}
		result.Attempts = attemptHistory
	}
	if fetchConfig.BrowserScript != "" && (result.Err != nil || result.Status == 403 || result.Status == 202 || result.Status == 429 || result.Status == 503 || strings.TrimSpace(result.Body) == "") {
		browser := browserFetch(raw, timeout)
		result.Attempts = append(result.Attempts, browser.Attempts...)
		if browser.Err == nil || browser.Body != "" {
			return browser
		}
	}
	return result
}

func atsMatchLegacy(raw string) map[string]string {
	parsed := parseURL(raw)
	if parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil
	}
	host := strings.ToLower(parsed.Hostname())
	segments := []string{}
	for _, value := range strings.Split(strings.Trim(parsed.Path, "/"), "/") {
		if value != "" {
			segments = append(segments, value)
		}
	}
	result := func(provider, identifier string) map[string]string {
		if identifier == "" {
			return nil
		}
		path := ""
		if len(segments) > 0 {
			path = "/" + segments[0]
		}
		canonicalHost := host
		if provider == "lever" && host == "api.lever.co" {
			canonicalHost = "jobs.lever.co"
		}
		if provider == "eightfold" {
			path = "/" + strings.Join(segments, "/")
		}
		if provider == "teamtailor" {
			path = ""
		}
		return map[string]string{"provider": provider, "identifier": identifier, "url": "https://" + canonicalHost + path, "raw_url": raw}
	}
	if host == "apply.workable.com" && len(segments) > 0 && segments[0] != "j" {
		return result("workable", segments[0])
	}
	if strings.HasSuffix(host, ".workable.com") {
		return result("workable", strings.TrimSuffix(host, ".workable.com"))
	}
	if blockedGreenhouseHosts[host] {
		return nil
	}
	if (host == "boards.greenhouse.io" || host == "job-boards.greenhouse.io" || strings.HasSuffix(host, ".greenhouse.io")) && len(segments) > 0 && segments[0] != "embed" && !blockedGreenhouseSegments[strings.ToLower(segments[0])] {
		return result("greenhouse", segments[0])
	}
	if host == "apply.careers.microsoft.com" || host == "explore.jobs.netflix.net" {
		if len(segments) == 0 || strings.ToLower(segments[0]) != "careers" {
			return nil
		}
		return result("eightfold", strings.Join(segments, "/"))
	}
	if host == "api.lever.co" && len(segments) >= 3 && segments[0] == "v0" && segments[1] == "postings" {
		return result("lever", segments[2])
	}
	if (host == "jobs.lever.co" || host == "jobs.eu.lever.co") && len(segments) > 0 {
		return result("lever", segments[0])
	}
	if (host == "jobs.smartrecruiters.com" || host == "careers.smartrecruiters.com") && len(segments) > 0 {
		index := 0
		if segments[0] == "ni" {
			index = 1
		}
		if len(segments) > index {
			return result("smartrecruiters", segments[index])
		}
	}
	if (host == "jobs.ashbyhq.com" || host == "ashbyhq.com") && len(segments) > 0 && segments[0] != "embed" && segments[0] != "api" {
		return result("ashby", segments[0])
	}
	providers := []struct{ suffix, name string }{{".teamtailor.com", "teamtailor"}, {".bamboohr.com", "bamboohr"}, {".pinpointhq.com", "pinpoint"}, {".recruitee.com", "recruitee"}, {".breezy.hr", "breezy"}, {".careers.hibob.com", "hibob"}, {".livevacancies.co.uk", "hireful"}, {".softgarden.io", "softgarden"}, {".hire.trakstar.com", "trakstar"}, {".talent-soft.com", "talent_soft"}, {".ttcportals.com", "ttcportals"}, {".schoolrecruiter.com", "schoolrecruiter"}}
	for _, provider := range providers {
		if strings.HasSuffix(host, provider.suffix) && host != strings.TrimPrefix(provider.suffix, ".") {
			subdomain := strings.TrimSuffix(host, provider.suffix)
			if blocked := blockedATSSubdomains[strings.TrimPrefix(provider.suffix, ".")]; blocked[subdomain] {
				return nil
			}
			if provider.name == "teamtailor" {
				if blockedTeamtailorSubdomains[subdomain] {
					return nil
				}
			}
			return result(provider.name, strings.TrimSuffix(host, provider.suffix))
		}
	}
	if strings.HasSuffix(host, ".myworkdayjobs.com") && len(segments) > 0 && segments[0] != "wday" {
		return result("workday_cxs", strings.TrimSuffix(host, ".myworkdayjobs.com"))
	}
	if host == "jobs.jobvite.com" && len(segments) > 0 {
		return result("jobvite", segments[0])
	}
	if host == "ats.rippling.com" && len(segments) > 0 {
		return result("rippling", segments[len(segments)-1])
	}
	if strings.HasSuffix(host, ".avature.net") && host != "avature.net" {
		tenant := strings.TrimSuffix(host, ".avature.net")
		if tenant == "" || strings.Contains(tenant, ".") || tenant == "www" || tenant == "www2" || tenant == "api" || tenant == "static" || tenant == "cdn" || tenant == "assets" {
			return nil
		}
		portalIndex := 0
		if len(segments) > 0 && isAvatureLocale(segments[0]) {
			if !strings.EqualFold(segments[0], "en_gb") {
				return nil
			}
			portalIndex = 1
		}
		identifier := tenant
		if len(segments) > portalIndex && !avatureActionSegments[strings.ToLower(segments[portalIndex])] {
			portal := segments[:minInt(len(segments), 2)]
			identifier += "/" + strings.Join(portal, "/")
		}
		path := ""
		if len(segments) > 0 {
			path = "/" + strings.Join(segments, "/")
		}
		return map[string]string{"provider": "avature", "identifier": identifier, "url": "https://" + host + path, "raw_url": raw}
	}
	if strings.HasSuffix(host, ".icims.com") && len(segments) > 0 && segments[0] == "jobs" {
		return result("icims", strings.TrimSuffix(host, ".icims.com"))
	}
	if (host == "careers-page.com" || host == "www.careers-page.com") && len(segments) > 0 {
		return result("careers_page", segments[0])
	}
	return nil
}

func atsMatch(raw string) map[string]string {
	raw = normalizeATSCandidate(raw)
	if raw == "" || atsIsBlocked(raw) {
		return nil
	}
	parsed := parseURL(raw)
	if parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil
	}
	host := strings.ToLower(parsed.Hostname())
	segments := atsPathSegments(raw)
	ok := func(provider, identifier string) map[string]string {
		if identifier == "" {
			return nil
		}
		return map[string]string{"provider": provider, "identifier": identifier, "url": canonicalizeATSURL(raw, provider), "raw_url": raw}
	}
	lowerSegment := func(index int) string {
		if index < 0 || index >= len(segments) {
			return ""
		}
		return strings.ToLower(segments[index])
	}

	if host == "apply.workable.com" {
		first := lowerSegment(0)
		if first != "" && first != "j" && !atsWorkableBlockedSegments[first] {
			return ok("workable", first)
		}
		return nil
	}
	if strings.HasSuffix(host, ".workable.com") {
		slug := strings.TrimSuffix(host, ".workable.com")
		if slug != "" && slug != "www" && slug != "apply" {
			return ok("workable", slug)
		}
		return nil
	}
	if blockedGreenhouseHosts[host] {
		return nil
	}
	if host == "boards.greenhouse.io" || host == "job-boards.greenhouse.io" || strings.HasSuffix(host, ".greenhouse.io") {
		board := ""
		if len(segments) > 0 {
			board = segments[0]
		}
		if strings.EqualFold(board, "embed") {
			board = parsed.Query().Get("for")
		}
		if board == "" || strings.EqualFold(board, "embed") || blockedGreenhouseSegments[strings.ToLower(board)] {
			return nil
		}
		return ok("greenhouse", board)
	}
	if host == "api.lever.co" {
		if len(segments) >= 3 && strings.EqualFold(segments[0], "v0") && strings.EqualFold(segments[1], "postings") && !atsLeverBlockedSegments[strings.ToLower(segments[2])] {
			return ok("lever", segments[2])
		}
		return nil
	}
	if host == "jobs.lever.co" || host == "jobs.eu.lever.co" {
		company := ""
		if len(segments) > 0 {
			company = segments[0]
		}
		if company != "" && !atsLeverBlockedSegments[strings.ToLower(company)] {
			return ok("lever", company)
		}
		return nil
	}
	if host == "jobs.smartrecruiters.com" || host == "careers.smartrecruiters.com" {
		index := 0
		if lowerSegment(0) == "ni" {
			index = 1
		}
		company := ""
		if index < len(segments) {
			company = segments[index]
		}
		if company != "" && !atsSmartRecruitersNonBoardSegments[strings.ToLower(company)] {
			return ok("smartrecruiters", company)
		}
		return nil
	}
	if host == "jobs.ashbyhq.com" || host == "ashbyhq.com" {
		org := ""
		if len(segments) > 0 {
			org = segments[0]
		}
		if org != "" && strings.ToLower(org) != "embed" && strings.ToLower(org) != "api" {
			return ok("ashby", org)
		}
		return nil
	}
	if host == "apply.careers.microsoft.com" || host == "explore.jobs.netflix.net" {
		if len(segments) == 0 || lowerSegment(0) == "careers" {
			identifier := strings.Join(segments, "/")
			if identifier == "" {
				identifier = "careers"
			}
			return ok("eightfold", identifier)
		}
		return nil
	}
	if strings.HasSuffix(host, ".teamtailor.com") {
		sub := strings.TrimSuffix(host, ".teamtailor.com")
		if sub != "" && !blockedTeamtailorSubdomains[sub] {
			return ok("teamtailor", sub)
		}
		return nil
	}
	if host == "jobs.jobvite.com" {
		company := ""
		if len(segments) > 0 {
			company = segments[0]
		}
		return ok("jobvite", company)
	}
	if host == "ats.rippling.com" {
		if len(segments) == 0 {
			return nil
		}
		index := 0
		if isRipplingLocale(lowerSegment(0)) {
			index = 1
		}
		if index >= len(segments) {
			return nil
		}
		return ok("rippling", strings.ToLower(segments[index]))
	}
	if strings.HasSuffix(host, ".myworkdayjobs.com") {
		tenant := strings.TrimSuffix(host, ".myworkdayjobs.com")
		if tenant != "" && tenant != "www" && len(segments) > 0 && lowerSegment(0) != "wday" {
			return ok("workday_cxs", tenant)
		}
		return nil
	}
	if host == "recruiting.paylocity.com" && len(segments) >= 4 && lowerSegment(0) == "recruiting" && lowerSegment(1) == "jobs" && atsPaylocityBoardRoutes[lowerSegment(2)] {
		return ok("paylocity", strings.ToLower(segments[2]+"/"+segments[3]))
	}
	if host == "recruiting.ultipro.com" && len(segments) >= 3 && lowerSegment(1) == "jobboard" {
		return ok("ultipro", strings.ToLower(segments[0]+"/"+segments[2]))
	}
	for _, provider := range []struct{ suffix, name string }{
		{".bamboohr.com", "bamboohr"}, {".pinpointhq.com", "pinpoint"}, {".recruitee.com", "recruitee"},
		{".breezy.hr", "breezy"}, {".careers.hibob.com", "hibob"}, {".livevacancies.co.uk", "hireful"},
	} {
		if strings.HasSuffix(host, provider.suffix) && host != strings.TrimPrefix(provider.suffix, ".") {
			sub := strings.TrimSuffix(host, provider.suffix)
			blocked := blockedATSSubdomains[strings.TrimPrefix(provider.suffix, ".")]
			if sub != "" && !blocked[sub] {
				return ok(provider.name, sub)
			}
			return nil
		}
	}
	if atsPersonioHostRE.MatchString(host) {
		sub := strings.SplitN(host, ".", 2)[0]
		if sub != "" && sub != "www" {
			return ok("personio", sub)
		}
		return nil
	}
	if host == "jobs.nhs.uk" || host == "beta.jobs.nhs.uk" || host == "www.jobs.nhs.uk" {
		query := strings.ToLower(parsed.RawQuery)
		bare := len(segments) == 0 || (len(segments) == 1 && lowerSegment(0) == "candidate") || (len(segments) == 2 && lowerSegment(0) == "candidate" && lowerSegment(1) == "search") || (len(segments) == 3 && lowerSegment(0) == "candidate" && lowerSegment(1) == "search" && lowerSegment(2) == "results")
		if strings.Contains(query, "employer=") || strings.Contains(query, "keyword=") || !bare {
			identifier := "nhs"
			if len(segments) > 0 {
				identifier = segments[0]
			} else if len(query) > 80 {
				identifier = query[:80]
			} else if query != "" {
				identifier = query
			}
			return ok("nhs_jobs", identifier)
		}
		return nil
	}
	if strings.HasSuffix(host, ".avature.net") && host != "avature.net" {
		tenant := strings.TrimSuffix(host, ".avature.net")
		if tenant == "" || strings.Contains(tenant, ".") || atsAvatureBlocked[tenant] {
			return nil
		}
		if len(segments) > 0 && isAvatureLocale(segments[0]) {
			if !strings.EqualFold(segments[0], "en_gb") {
				return nil
			}
		}
		portalIndex := 0
		if len(segments) > 0 && isAvatureLocale(segments[0]) {
			portalIndex = 1
		}
		portalSegment := lowerSegment(portalIndex)
		portal := strings.Join(segments[:minInt(len(segments), 2)], "/")
		identifier := tenant
		if !avatureActionSegments[portalSegment] && portal != "" {
			identifier += "/" + portal
		}
		return ok("avature", identifier)
	}
	if host == "employmenthero.com" || host == "www.employmenthero.com" {
		path := "/" + strings.Join(segments, "/")
		match := regexp.MustCompile(`(?i)(?:^|/)jobs/organisations/([a-z0-9-]+)$`).FindStringSubmatch(path)
		if len(match) > 1 {
			return ok("employment_hero", strings.ToLower(match[1]))
		}
		return nil
	}
	if strings.HasSuffix(host, ".dayforcehcm.com") && host != "dayforcehcm.com" && len(segments) >= 3 {
		if regexp.MustCompile(`(?i)^[a-z]{2}(?:-[a-z]{2})?$`).MatchString(segments[0]) && regexp.MustCompile(`(?i)^[a-z0-9-]+$`).MatchString(segments[1]) && regexp.MustCompile(`(?i)^[a-z0-9-]+$`).MatchString(segments[2]) {
			return ok("dayforce", strings.ToLower(segments[1]+"/"+segments[2]))
		}
		return nil
	}
	if strings.HasSuffix(host, ".icims.com") && host != "icims.com" && lowerSegment(0) == "jobs" {
		return ok("icims", strings.TrimSuffix(host, ".icims.com"))
	}
	if strings.HasSuffix(host, ".ciphr-irecruit.com") && host != "ciphr-irecruit.com" {
		path := "/" + strings.Join(segments, "/")
		if regexp.MustCompile(`(?i)/(?:templates/CIPHR|applicants/vacancy)(?:/|$)`).MatchString(path) {
			return ok("ciphr_irecruit", strings.TrimSuffix(host, ".ciphr-irecruit.com"))
		}
		return nil
	}
	for _, suffix := range atsNetworxSuffixes {
		if host == strings.TrimPrefix(suffix, ".") || strings.HasSuffix(host, suffix) {
			sub := host
			if strings.HasSuffix(host, suffix) {
				sub = strings.TrimSuffix(host, suffix)
			}
			if atsNetworxBlockedHosts[sub] || strings.Contains(sub, ".") {
				return nil
			}
			clientID := parsed.Query().Get("cid")
			identifier := host
			if _, err := strconv.Atoi(clientID); err == nil && clientID != "" {
				identifier = "cid:" + clientID
			}
			return ok("networx", identifier)
		}
	}
	if strings.HasSuffix(host, ".softgarden.io") && host != "softgarden.io" {
		path := "/" + strings.Join(segments, "/")
		if len(segments) == 0 || regexp.MustCompile(`(?i)/(?:job|vacancies)(?:/|$)`).MatchString(path) {
			return ok("softgarden", strings.TrimSuffix(host, ".softgarden.io"))
		}
		return nil
	}
	if strings.HasSuffix(host, ".hire.trakstar.com") && host != "hire.trakstar.com" && lowerSegment(0) == "jobs" {
		return ok("trakstar", strings.TrimSuffix(host, ".hire.trakstar.com"))
	}
	if host == "careers-page.com" || host == "www.careers-page.com" {
		slug := lowerSegment(0)
		if slug != "" && regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`).MatchString(slug) {
			return ok("careers_page", slug)
		}
		return nil
	}
	if strings.HasSuffix(host, ".talent-soft.com") && host != "talent-soft.com" {
		path := "/" + strings.Join(segments, "/")
		if regexp.MustCompile(`(?i)^/(?:job|stelle|accueil\.aspx)(?:/|$)`).MatchString(path) || regexp.MustCompile(`(?i)^/job/list-of-all-jobs\.aspx`).MatchString(path) {
			return ok("talent_soft", strings.TrimSuffix(host, ".talent-soft.com"))
		}
		return nil
	}
	if strings.HasSuffix(host, ".ttcportals.com") && host != "ttcportals.com" && (lowerSegment(0) == "search" || lowerSegment(0) == "jobs") {
		return ok("ttcportals", strings.TrimSuffix(host, ".ttcportals.com"))
	}
	if strings.HasSuffix(host, ".schoolrecruiter.com") && host != "schoolrecruiter.com" && (len(segments) == 0 || lowerSegment(0) == "job" || lowerSegment(0) == "jobseekers") {
		return ok("schoolrecruiter", strings.TrimSuffix(host, ".schoolrecruiter.com"))
	}
	return nil
}

var atsAvatureBlocked = map[string]bool{"www": true, "www2": true, "api": true, "static": true, "cdn": true, "assets": true}

func teamtailorParentCareersURL(body, customHost string) string {
	for _, rawURL := range atsAbsURLRE.FindAllString(body, -1) {
		parsed := parseURL(normalizeATSCandidate(rawURL))
		if parsed == nil || strings.EqualFold(parsed.Hostname(), customHost) || !strings.HasPrefix(strings.ToLower(parsed.Hostname()), "careers.") || strings.TrimRight(parsed.Path, "/") != "/cookie-policy" {
			continue
		}
		return "https://" + strings.ToLower(parsed.Hostname()) + "/jobs"
	}
	return ""
}

func teamtailorCustomDomainMatch(body, baseURL string) map[string]string {
	if body == "" || baseURL == "" || !strings.Contains(strings.ToLower(body), "teamtailor") {
		return nil
	}
	rssRE := regexp.MustCompile(`(?is)<link\b[^>]+rel=["'][^"']*alternate[^"']*["'][^>]+type=["']application/rss\+xml["'][^>]+href=["'][^"']*/jobs\.rss["']`)
	if !rssRE.MatchString(body) {
		return nil
	}
	customHost := hostname(baseURL)
	if customHost == "" || strings.HasSuffix(customHost, ".teamtailor.com") {
		return nil
	}
	fontRE := regexp.MustCompile(`(?i)fonts\.teamtailor-cdn\.com/teamtailor-production/[a-z0-9][a-z0-9-]*?(?:-\d+)?/custom-fonts\.css`)
	companyRE := regexp.MustCompile(`(?i)https?://app\.teamtailor\.com/companies/([a-z0-9_-]+@[a-z0-9_-]+)(?:[/'?\s]|$)`)
	careersiteRE := regexp.MustCompile(`(?i)(?:assets(?:-[a-z0-9-]+)?\.teamtailor-cdn\.com/[^"'<> ]*careersite-[^"'<> ]+\.css|data-careersite--ready)`)
	atsURL := ""
	identifier := customHost
	if fontRE.MatchString(body) {
		atsURL = teamtailorParentCareersURL(body, customHost)
	} else {
		company := companyRE.FindStringSubmatch(body)
		if len(company) == 0 && !careersiteRE.MatchString(body) {
			return nil
		}
		if len(company) > 1 {
			identifier = company[1]
		}
		atsURL = teamtailorParentCareersURL(body, customHost)
	}
	if atsURL == "" {
		atsURL = "https://" + customHost + "/"
	} else {
		identifier = hostname(atsURL)
	}
	return map[string]string{"provider": "teamtailor", "identifier": identifier, "url": atsURL, "raw_url": baseURL}
}

// Pinpoint career sites may use the employer's own hostname. Their HTML still
// exposes the platform's RSS feed and assets, but there is no pinpoint-hq URL
// for atsMatch to recognise. Keep the employer URL so the downstream parser
// can use the same custom-domain feed endpoint.
func pinpointCustomDomainMatch(body, baseURL string) map[string]string {
	if body == "" || baseURL == "" {
		return nil
	}
	lower := strings.ToLower(body)
	if !strings.Contains(lower, "pinpointhq") || !strings.Contains(lower, "jobs.rss") {
		return nil
	}
	customHost := hostname(baseURL)
	if customHost == "" || strings.HasSuffix(customHost, ".pinpointhq.com") {
		return nil
	}
	return map[string]string{"provider": "pinpoint", "identifier": customHost, "url": "https://" + customHost + "/", "raw_url": baseURL}
}

func extractATSCandidatesFromHTML(body, baseURL, sourceKind string) []map[string]string {
	if body == "" {
		return nil
	}
	candidates := []map[string]string{}
	seen := map[string]bool{}
	add := func(raw, discovery string) {
		absolute := absoluteURL(raw, baseURL)
		if absolute == "" {
			absolute = raw
		}
		match := atsMatch(absolute)
		if match == nil {
			return
		}
		key := strings.ToLower(match["url"])
		if seen[key] {
			return
		}
		seen[key] = true
		match["discovery"] = discovery
		match["source_kind"] = sourceKind
		candidates = append(candidates, match)
	}
	for _, anchor := range parseHTML(body, baseURL).Anchors {
		add(anchor.Href, "href")
	}
	for _, raw := range atsAbsURLRE.FindAllString(body, -1) {
		add(raw, "raw")
	}
	if custom := teamtailorCustomDomainMatch(body, baseURL); custom != nil {
		key := strings.ToLower(custom["url"])
		if !seen[key] {
			custom["discovery"] = "custom"
			custom["source_kind"] = sourceKind
			candidates = append(candidates, custom)
		}
	}
	if custom := pinpointCustomDomainMatch(body, baseURL); custom != nil {
		key := strings.ToLower(custom["url"])
		if !seen[key] {
			custom["discovery"] = "custom"
			custom["source_kind"] = sourceKind
			candidates = append(candidates, custom)
		}
	}
	return candidates
}

func pickBestATS(matches []map[string]string) map[string]string {
	if len(matches) == 0 {
		return nil
	}
	available := []map[string]string{}
	allCheckedInactive := true
	for _, item := range matches {
		if item["availability_checked"] != "true" {
			allCheckedInactive = false
		}
		if item["active"] == "true" {
			available = append(available, item)
			allCheckedInactive = false
		}
	}
	if len(available) > 0 {
		matches = available
	} else if allCheckedInactive {
		return nil
	}
	sourceRank := map[string]int{"external_careers": 0, "careers_page": 0, "vacancies_page": 0, "page_link": 0, "website": 1}
	discoveryRank := map[string]int{"href": 0, "custom": 0, "raw": 1}
	sort.SliceStable(matches, func(left, right int) bool {
		leftURL, rightURL := matches[left]["url"], matches[right]["url"]
		leftPath, rightPath := len(atsPathSegments(leftURL)), len(atsPathSegments(rightURL))
		leftQuery, rightQuery := 0, 0
		if parsed := parseURL(matches[left]["raw_url"]); parsed != nil && parsed.RawQuery != "" {
			leftQuery = 1
		}
		if parsed := parseURL(matches[right]["raw_url"]); parsed != nil && parsed.RawQuery != "" {
			rightQuery = 1
		}
		leftKey := []int{sourceRank[matches[left]["source_kind"]], discoveryRank[matches[left]["discovery"]], leftPath, leftQuery, len(leftURL)}
		rightKey := []int{sourceRank[matches[right]["source_kind"]], discoveryRank[matches[right]["discovery"]], rightPath, rightQuery, len(rightURL)}
		for index := range leftKey {
			if leftKey[index] != rightKey[index] {
				return leftKey[index] < rightKey[index]
			}
		}
		return leftURL < rightURL
	})
	return matches[0]
}

var atsIdentityStopWords = map[string]bool{
	"about": true, "apply": true, "career": true, "careers": true, "company": true, "contact": true,
	"current": true, "employment": true, "home": true, "hiring": true, "join": true, "jobs": true,
	"opportunities": true, "our": true, "people": true, "roles": true, "site": true, "team": true,
	"the": true, "vacancies": true, "website": true, "work": true, "with": true, "www": true,
	"ac": true, "au": true, "biz": true, "ca": true, "co": true, "com": true, "edu": true,
	"gov": true, "ie": true, "info": true, "io": true, "me": true, "net": true, "org": true, "uk": true, "us": true,
}
var atsIdentityTokenRE = regexp.MustCompile(`[a-z0-9]+`)

func atsIdentityTokens(values ...string) map[string]bool {
	result := map[string]bool{}
	for _, value := range values {
		for _, token := range atsIdentityTokenRE.FindAllString(strings.ToLower(value), -1) {
			if len(token) >= 3 && !atsIdentityStopWords[token] {
				result[token] = true
			}
		}
	}
	return result
}

func externalCareersIdentityMatches(sourceURL string, source Page, externalURL string, external Page) bool {
	sourceHost, externalHost := hostname(sourceURL), hostname(externalURL)
	sourceValues := []string{sourceHost, source.Title}
	externalValues := []string{externalHost, external.Title}
	for _, value := range source.Headings {
		if len(sourceValues) >= 5 {
			break
		}
		sourceValues = append(sourceValues, value)
	}
	for _, value := range external.Headings {
		if len(externalValues) >= 5 {
			break
		}
		externalValues = append(externalValues, value)
	}
	sourceTokens, externalTokens := atsIdentityTokens(sourceValues...), atsIdentityTokens(externalValues...)
	for token := range sourceTokens {
		if externalTokens[token] {
			return true
		}
	}
	return false
}

func candidateURLs(candidates []map[string]string) []interface{} {
	urls := make([]interface{}, 0, len(candidates))
	for _, candidate := range candidates {
		urls = append(urls, candidate["url"])
	}
	return urls
}

type atsCandidateFetcher func(string, time.Duration) FetchResult

func atsStatusIsLive(status int) bool {
	return status >= http.StatusOK && status < http.StatusBadRequest
}

func avatureMeta(body, name string) string {
	pattern := regexp.MustCompile(`(?is)<meta[^>]+name=["']` + regexp.QuoteMeta(name) + `["'][^>]+content=["']([^"']*)`)
	match := pattern.FindStringSubmatch(body)
	if len(match) < 2 {
		return ""
	}
	return strings.Trim(strings.TrimSpace(match[1]), "/")
}

func atsFinalHostMatches(result FetchResult, expected string) bool {
	finalHost := hostname(result.FinalURL)
	return finalHost != "" && strings.EqualFold(finalHost, expected)
}

func atsFinalHostHasSuffix(result FetchResult, suffix string) bool {
	finalHost := hostname(result.FinalURL)
	return finalHost != "" && (finalHost == strings.TrimPrefix(suffix, ".") || strings.HasSuffix(finalHost, suffix))
}

func verifyAvatureCandidate(candidate map[string]string, initial FetchResult, fetch atsCandidateFetcher, timeout time.Duration) (bool, string) {
	if !atsStatusIsLive(initial.Status) {
		return false, fmt.Sprintf("HTTP %d", initial.Status)
	}
	if !atsFinalHostHasSuffix(initial, ".avature.net") {
		return false, "redirected off avature.net"
	}
	body := initial.Body
	if locale := avatureMeta(body, "avature.portal.lang"); locale != "" && !strings.EqualFold(locale, "en_gb") {
		return false, "non-uk avature locale: " + locale
	}
	parsed := parseURL(candidate["raw_url"])
	if parsed == nil {
		return false, "malformed Avature URL"
	}
	segments := []string{}
	for _, segment := range strings.Split(strings.Trim(parsed.Path, "/"), "/") {
		if segment != "" {
			segments = append(segments, segment)
		}
	}
	portalIndex := 0
	if len(segments) > 0 && isAvatureLocale(segments[0]) {
		portalIndex = 1
	}
	portal := ""
	if meta := avatureMeta(body, "avature.portal.urlPath"); meta != "" {
		portal = meta
	} else if len(segments) > portalIndex {
		portal = segments[portalIndex]
	}
	if portal == "" || avatureActionSegments[strings.ToLower(portal)] {
		return false, "Avature page has no job-board portal"
	}
	scheme := parsed.Scheme
	if scheme == "" {
		scheme = "https"
	}
	searchURL := scheme + "://" + parsed.Hostname() + "/" + portal + "/SearchJobs/"
	search := fetch(searchURL, timeout)
	if !atsStatusIsLive(search.Status) {
		return false, fmt.Sprintf("Avature SearchJobs HTTP %d", search.Status)
	}
	if !atsFinalHostHasSuffix(search, ".avature.net") {
		return false, "Avature SearchJobs redirected off provider"
	}
	return true, ""
}

func verifyATSCandidatesWith(candidates []map[string]string, timeout time.Duration, fetch atsCandidateFetcher) []map[string]string {
	verified := make([]map[string]string, 0, len(candidates))
	for _, candidate := range candidates {
		item := map[string]string{}
		for key, value := range candidate {
			item[key] = value
		}
		initial := fetch(item["url"], timeout)
		statusResult := initial
		active := atsStatusIsLive(initial.Status)
		errorMessage := ""
		provider := item["provider"]
		switch provider {
		case "avature":
			active, errorMessage = verifyAvatureCandidate(item, initial, fetch, timeout)
		case "bamboohr":
			candidateURL := parseURL(item["url"])
			if candidateURL == nil {
				active, errorMessage = false, "malformed BambooHR URL"
				break
			}
			probe := fetch("https://"+candidateURL.Hostname()+"/careers/list", timeout)
			statusResult = probe
			active = atsStatusIsLive(probe.Status) && atsFinalHostMatches(probe, candidateURL.Hostname())
			if !active {
				errorMessage = "BambooHR careers endpoint unavailable or redirected"
			}
		case "ashby":
			parts := strings.Split(strings.Trim(item["url"], "/"), "/")
			org := parts[len(parts)-1]
			probe := fetch("https://api.ashbyhq.com/posting-api/job-board/"+org, timeout)
			statusResult = probe
			payload := map[string]interface{}{}
			decodeErr := json.Unmarshal([]byte(probe.Body), &payload)
			_, hasJobs := payload["jobs"].([]interface{})
			active = atsStatusIsLive(probe.Status) && decodeErr == nil && hasJobs
			if !active {
				errorMessage = "Ashby posting API unavailable"
			}
		case "greenhouse":
			parts := strings.Split(strings.Trim(item["url"], "/"), "/")
			board := parts[len(parts)-1]
			probe := fetch("https://boards-api.greenhouse.io/v1/boards/"+board+"/jobs", timeout)
			statusResult = probe
			payload := map[string]interface{}{}
			decodeErr := json.Unmarshal([]byte(probe.Body), &payload)
			_, hasJobs := payload["jobs"].([]interface{})
			active = atsStatusIsLive(probe.Status) && decodeErr == nil && hasJobs
			if !active {
				errorMessage = "Greenhouse jobs API unavailable"
			}
		default:
			if !active {
				errorMessage = fmt.Sprintf("HTTP %d", initial.Status)
			}
		}
		item["availability_checked"] = "true"
		item["http_status"] = strconv.Itoa(statusResult.Status)
		if active {
			item["active"] = "true"
		} else {
			item["active"] = "false"
			if errorMessage == "" {
				errorMessage = fmt.Sprintf("HTTP %d", initial.Status)
			}
			item["error"] = errorMessage
		}
		verified = append(verified, item)
	}
	return verified
}

func verifyATSCandidates(candidates []map[string]string, timeout time.Duration) []map[string]string {
	return verifyATSCandidatesWith(candidates, timeout, fetchPage)
}

var limitedContextWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "being": true, "by": true, "but": true, "changes": true,
	"content": true, "correct": true, "date": true, "for": true, "from": true,
	"including": true, "in": true, "is": true, "it": true, "its": true,
	"law": true, "licensed": true, "not": true, "of": true, "on": true,
	"operated": true, "or": true, "our": true, "terms": true, "that": true,
	"the": true, "their": true, "this": true, "to": true, "was": true,
	"were": true, "which": true, "with": true, "written": true, "your": true,
}

type limitedCompanyEvidence struct {
	Names  []string
	Counts []map[string]interface{}
}

func normalizedLimitedName(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, " \t\r\n,.;:|\"'“”‘’()[]{}<>©")
	return strings.TrimSpace(spaceRE.ReplaceAllString(value, " "))
}

func companyWord(value string) bool {
	value = strings.Trim(value, " \t\r\n,.;:|\"'“”‘’()[]{}<>©")
	if value == "" {
		return false
	}
	runes := []rune(value)
	return unicode.IsUpper(runes[0]) || unicode.IsDigit(runes[0]) || strings.ToUpper(value) == value
}

func limitedCandidate(before, suffix string) string {
	// A legal-form word is often preceded by boilerplate such as "operated by"
	// or "including but not". Start after the nearest sentence/HTML boundary,
	// then discard leading context words while retaining the name itself.
	// If the page has flattened adjacent text nodes without punctuation, also
	// start after the previous legal-form match so two nearby names do not get
	// concatenated into one candidate.
	if matches := limitedRE.FindAllStringIndex(before, -1); len(matches) > 0 {
		before = before[matches[len(matches)-1][1]:]
	}
	if index := strings.LastIndexAny(before, ".!?;:()[]{}<>©|,\"'“”‘’"); index >= 0 {
		before = before[index+1:]
	}
	words := strings.Fields(before)
	if len(words) > 8 {
		words = words[len(words)-8:]
	}
	words = append(words, suffix)
	start := 0
	for start < len(words)-1 && limitedContextWords[strings.ToLower(strings.Trim(words[start], ",.;:|\"'“”‘’()[]{}<>©"))] {
		start++
	}
	// Lowercase prose immediately followed by a title-cased word is usually
	// the tail of the sentence, not part of the company name.
	for start < len(words)-1 && !companyWord(words[start]) && companyWord(words[start+1]) {
		start++
	}
	if start >= len(words)-1 {
		return ""
	}
	return normalizedLimitedName(strings.Join(words[start:], " "))
}

func limitedCompanyEvidenceFromText(text string) limitedCompanyEvidence {
	type counted struct {
		name  string
		count int
		first int
	}
	byKey := map[string]*counted{}
	for position, match := range limitedRE.FindAllStringIndex(text, -1) {
		name := limitedCandidate(text[:match[0]], text[match[0]:match[1]])
		parts := strings.Fields(name)
		if len(parts) < 2 {
			continue
		}
		key := strings.ToLower(strings.Join(parts, " "))
		if strings.Contains(key, "not limited") || strings.Contains(key, "limited to") || strings.Contains(key, "limited liability") {
			continue
		}
		item := byKey[key]
		if item == nil {
			item = &counted{name: name, first: position}
			byKey[key] = item
		}
		item.count++
	}
	all := make([]*counted, 0, len(byKey))
	for _, item := range byKey {
		all = append(all, item)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].count != all[j].count {
			return all[i].count > all[j].count
		}
		return all[i].first < all[j].first
	})
	if len(all) > 25 {
		all = all[:25]
	}
	evidence := limitedCompanyEvidence{Names: make([]string, 0, len(all)), Counts: make([]map[string]interface{}, 0, len(all))}
	for _, item := range all {
		evidence.Names = append(evidence.Names, item.name)
		evidence.Counts = append(evidence.Counts, map[string]interface{}{"name": item.name, "count": item.count})
	}
	return evidence
}

func limitedCompanyNames(text string) []string {
	return limitedCompanyEvidenceFromText(text).Names
}
func joinValues(values []map[string]interface{}, key string) interface{} {
	out := []string{}
	for _, value := range values {
		if text, ok := value[key].(string); ok {
			out = append(out, text)
		}
	}
	out = uniqFold(out, 0)
	if len(out) == 0 {
		return nil
	}
	return strings.Join(out, ", ")
}
func nilString(value string) interface{} {
	if value == "" {
		return nil
	}
	return value
}
func nilSlice(value interface{}) interface{} {
	if value == nil {
		return nil
	}
	return value
}
func jsonValue(value interface{}) interface{} {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func activeHTTPStatus(status int) bool {
	return (status >= http.StatusOK && status < http.StatusBadRequest) || status == http.StatusUnauthorized || status == http.StatusForbidden
}

func extractOne(raw string, timeout time.Duration, recursive bool) map[string]interface{} {
	started := time.Now()
	fetched := fetchPage(raw, timeout)
	status, finalURL, body, fetchErr := fetched.Status, fetched.FinalURL, fetched.Body, fetched.Err
	if finalURL == "" {
		finalURL = raw
	}
	finalURL = stripDefaultPort(finalURL)
	page := parseHTML(body, finalURL)
	host := hostname(finalURL)
	hosting := detectHosting(body, finalURL)
	pageLinks := map[string]string{}
	for field, keywords := range pageKeywords {
		if value := findPageLink(page.Anchors, keywords, finalURL); value != "" {
			pageLinks[field] = value
		}
	}
	ats := extractATSCandidatesFromHTML(body, finalURL, "website")
	atsSeen := map[string]bool{}
	for _, candidate := range ats {
		atsSeen[strings.ToLower(candidate["url"])] = true
	}
	externalATSIdentityRejections := []interface{}{}
	// A registrar/parking/lander response cannot provide useful company or ATS
	// evidence. Avoid recursive requests once the root response is classified.
	if recursive && !hosting {
		visited := map[string]bool{finalURL: true}
		queue := []struct{ url, label string }{}
		for _, item := range []struct{ field, label string }{{"about_page_link", "about_page"}, {"contact_page_link", "contact_page"}, {"careers_page_link", "careers_page"}, {"vacancies_page_link", "vacancies_page"}, {"terms_and_conditions_page_link", "terms_page"}, {"privacy_policy_page_link", "privacy_page"}, {"legal_page_link", "legal_page"}} {
			if value := pageLinks[item.field]; value != "" && sameHost(value, finalURL) {
				queue = append(queue, struct{ url, label string }{value, item.label})
			}
		}
		for len(queue) > 0 && len(visited) < maxPages {
			item := queue[0]
			queue = queue[1:]
			if visited[item.url] {
				continue
			}
			visited[item.url] = true
			childFetched := fetchPage(item.url, timeout)
			childFinal, childBody, childErr := childFetched.FinalURL, childFetched.Body, childFetched.Err
			if childErr != nil && childBody == "" {
				continue
			}
			child := parseHTML(childBody, childFinal)
			for _, candidate := range extractATSCandidatesFromHTML(childBody, childFinal, item.label) {
				key := strings.ToLower(candidate["url"])
				if key != "" && !atsSeen[key] {
					atsSeen[key] = true
					ats = append(ats, candidate)
				}
			}
			page.Text += " " + child.Text
			page.FooterText += " " + child.FooterText
			page.Headings = append(page.Headings, child.Headings...)
			page.Anchors = append(page.Anchors, child.Anchors...)
			page.Emails = append(page.Emails, child.Emails...)
			page.Phones = append(page.Phones, child.Phones...)
			for key, value := range child.Structured {
				current, _ := page.Structured[key].([]string)
				incoming, _ := value.([]string)
				page.Structured[key] = append(current, incoming...)
			}
			for field, keywords := range pageKeywords {
				if _, exists := pageLinks[field]; !exists {
					if value := findPageLink(child.Anchors, keywords, childFinal); value != "" && sameHost(value, finalURL) {
						pageLinks[field] = value
						queue = append(queue, struct{ url, label string }{value, field})
					}
				}
			}
		}
		page.Text, page.FooterText = cleanText(page.Text), cleanText(page.FooterText)
		page.Headings = uniq(page.Headings, 25)
	}
	if recursive && !hosting {
		externalVisited := map[string]bool{}
		for _, field := range []string{"careers_page_link", "vacancies_page_link"} {
			pageURL := pageLinks[field]
			if pageURL == "" || hostname(pageURL) == host || atsMatch(pageURL) != nil || externalVisited[pageURL] {
				continue
			}
			externalVisited[pageURL] = true
			externalFetched := fetchPage(pageURL, timeout)
			externalFinal := externalFetched.FinalURL
			if externalFinal == "" {
				externalFinal = pageURL
			}
			if externalFetched.Err != nil && externalFetched.Body == "" {
				continue
			}
			externalPage := parseHTML(externalFetched.Body, externalFinal)
			externalCandidates := extractATSCandidatesFromHTML(externalFetched.Body, externalFinal, "external_careers")
			if externalCareersIdentityMatches(finalURL, page, pageURL, externalPage) {
				for _, candidate := range externalCandidates {
					key := strings.ToLower(candidate["url"])
					if key != "" && !atsSeen[key] {
						atsSeen[key] = true
						ats = append(ats, candidate)
					}
				}
			} else if len(externalCandidates) > 0 {
				externalATSIdentityRejections = append(externalATSIdentityRejections, map[string]interface{}{"page": pageURL, "ats": candidateURLs(externalCandidates)})
			}
			for _, childField := range []string{"careers_page_link", "vacancies_page_link"} {
				childURL := findPageLink(externalPage.Anchors, pageKeywords[childField], externalFinal)
				if childURL == "" || hostname(childURL) != hostname(externalFinal) || externalVisited[childURL] {
					continue
				}
				externalVisited[childURL] = true
				childFetched := fetchPage(childURL, timeout)
				childFinal := childFetched.FinalURL
				if childFinal == "" {
					childFinal = childURL
				}
				if childFetched.Err != nil && childFetched.Body == "" {
					continue
				}
				childPage := parseHTML(childFetched.Body, childFinal)
				childCandidates := extractATSCandidatesFromHTML(childFetched.Body, childFinal, "external_careers")
				if externalCareersIdentityMatches(finalURL, page, childURL, childPage) {
					for _, candidate := range childCandidates {
						key := strings.ToLower(candidate["url"])
						if key != "" && !atsSeen[key] {
							atsSeen[key] = true
							ats = append(ats, candidate)
						}
					}
				} else if len(childCandidates) > 0 {
					externalATSIdentityRejections = append(externalATSIdentityRejections, map[string]interface{}{"page": childURL, "ats": candidateURLs(childCandidates)})
				}
			}
		}
	}
	social := map[string]string{}
	for name, domains := range socialDomains {
		if value := findSocial(page.Anchors, domains, finalURL); value != "" {
			social[name+"_link"] = value
		}
	}
	emailsFound := []map[string]interface{}{}
	emailSeen := map[string]bool{}
	for _, link := range page.Emails {
		value := cleanEmail(link["email"])
		if value == "" || emailSeen[strings.ToLower(value)] {
			continue
		}
		emailSeen[strings.ToLower(value)] = true
		emailsFound = append(emailsFound, map[string]interface{}{"email": value, "source_page": classifyPage(link["source_page"]), "type": "general", "contactable_for_job": false, "key_person": nil, "key_person_job_title": nil})
	}
	for _, value := range getEmails(page.Text) {
		if emailSeen[strings.ToLower(value)] {
			continue
		}
		emailSeen[strings.ToLower(value)] = true
		emailsFound = append(emailsFound, map[string]interface{}{"email": value, "source_page": "website", "type": "general", "contactable_for_job": false, "key_person": nil, "key_person_job_title": nil})
	}
	phonesFound := []map[string]interface{}{}
	phoneSeen := map[string]bool{}
	for _, link := range page.Phones {
		value := cleanPhone(link["number"])
		if validPhone(value) && !phoneSeen[value] {
			phoneSeen[value] = true
			phonesFound = append(phonesFound, map[string]interface{}{"number": value, "country_code": "GB", "source_page": classifyPage(link["source_page"]), "type": "general", "contactable_for_job": false, "key_person": nil, "key_person_job_title": nil})
		}
	}
	for _, value := range getPhones(page.Text) {
		if phoneSeen[value] {
			continue
		}
		phoneSeen[value] = true
		phonesFound = append(phonesFound, map[string]interface{}{"number": value, "country_code": "GB", "source_page": "website", "type": "general", "contactable_for_job": false, "key_person": nil, "key_person_job_title": nil})
	}
	for _, field := range []string{"careers_page_link", "vacancies_page_link"} {
		if pageURL := pageLinks[field]; pageURL != "" {
			if match := atsMatch(pageURL); match != nil {
				match["discovery"] = "href"
				match["source_kind"] = "page_link"
				key := strings.ToLower(match["url"])
				if !atsSeen[key] {
					atsSeen[key] = true
					ats = append(ats, match)
				}
				delete(pageLinks, field)
			}
		}
	}
	ats = verifyATSCandidates(ats, timeout)
	if recursive {
		_ = maxPages
	} // recursive subpage transport is added below the direct parity baseline.
	location, language := "", detectLanguage(page.Text)
	if strings.HasSuffix(host, ".uk") {
		location = "GB"
	}
	// Receiving a valid web response is proof of a live HTTP service even when
	// the body is truncated or malformed. Keep content-based detection for
	// normal responses, but do not turn a 2xx/3xx/401/403 into inactive merely
	// because body reading or HTML parsing failed afterward.
	active := false
	if status != 0 {
		// Once an HTTP status is available, trust the status class rather than
		// page text. Error pages can contain words that make content detection
		// look like a live company site.
		active = activeHTTPStatus(status)
	} else if fetchErr == nil {
		active = detectActive(body, page)
	}
	active = active && !hosting
	websitePlatform, websitePlatforms := detectWebsitePlatforms(body)
	postcodes, registrations := getPostcodes(page.Text), getRegistrationNumbers(page.Text)
	extra := map[string]interface{}{"post_code": postcodes, "company_number": registrations}
	limitedEvidence := limitedCompanyEvidenceFromText(page.Text)
	limited := limitedEvidence.Names
	bestATS := pickBestATS(ats)
	if bestATS == nil {
		bestATS = map[string]string{}
	}
	legalNames := structuredStrings(page.Structured["legal_names"])
	structuredNames := structuredStrings(page.Structured["names"])
	tradingNames := structuredStrings(page.Structured["alternate_names"])
	companyName := firstNonEmpty(legalNames, structuredNames, tradingNames, limited)
	identity := map[string]interface{}{"company_name": nilString(companyName), "legal_names": legalNames, "limited_company_names": limited, "limited_company_name_counts": limitedEvidence.Counts, "trading_names": tradingNames, "company_numbers": registrations, "postcodes": postcodes, "towns": structuredStrings(page.Structured["towns"]), "addresses": structuredStrings(page.Structured["addresses"]), "structured_names": structuredNames, "structured_data_types": structuredStrings(page.Structured["types"]), "structured_data_present": true, "footer_evidence": strings.Contains(strings.ToLower(page.FooterText), "copyright"), "legal_page_evidence": false, "pages_inspected": []string{finalURL}, "page_kinds": []string{}}
	description := ""
	if parsed := parseURL(finalURL); parsed != nil && (parsed.Path == "" || parsed.Path == "/") {
		parts := []string{}
		if page.Title != "" {
			parts = append(parts, page.Title)
		}
		if page.Description != "" && !strings.Contains(page.Title, page.Description) {
			parts = append(parts, page.Description)
		}
		description = strings.TrimSpace(strings.Join(parts, " — "))
		if len(description) > 500 {
			description = description[:500]
		}
	}
	var atsTraceable interface{}
	if status == http.StatusOK {
		atsTraceable = len(bestATS) > 0
	}
	detail := map[string]interface{}{"website": nilString(finalURL), "active": active, "hosting": hosting, "location": nilString(location), "website_language": nilString(language), "logo_link": nilString(firstImage(page.Images, finalURL)), "twitter": nilString(social["twitter_link"]), "facebook": nilString(social["facebook_link"]), "linkedin": nilString(social["linkedin_link"]), "instagram": nilString(social["instagram_link"]), "youtube": nilString(social["youtube_link"]), "tiktok": nilString(social["tiktok_link"]), "contact_page": nilString(pageLinks["contact_page_link"]), "careers_page": nilString(pageLinks["careers_page_link"]), "vacancies_page": nilString(pageLinks["vacancies_page_link"]), "ats_page": nilString(bestATS["url"]), "terms_conditions_page": nilString(pageLinks["terms_and_conditions_page_link"]), "privacy_page": nilString(pageLinks["privacy_policy_page_link"]), "company_name": nilString(companyName), "company_description": nilString(description), "email": joinValues(emailsFound, "email"), "email_protected": strings.Contains(strings.ToLower(page.Text), "[email protected]"), "phone": joinValues(phonesFound, "number"), "new_emails": nilSlice(emailsFound), "new_phones": nilSlice(phonesFound), "extra_info": jsonValue(extra), "ats_traceable": atsTraceable}
	meta := map[string]interface{}{"input_url": raw, "final_url": finalURL, "http_status": statusOrNil(status), "fetch_time_sec": time.Since(started).Seconds(), "job_category_hint": 11, "ats_provider": nilString(bestATS["provider"]), "ats_identifier": nilString(bestATS["identifier"]), "ats_candidates": ats, "error": errorOrNil(fetchErr), "fetch_via": fetched.Via, "fetch_attempts": fetched.Attempts, "truncated": false, "website_platform": nilString(websitePlatform), "website_platforms": websitePlatforms, "limited_company": firstOrNil(limited), "identity_evidence": identity, "external_ats_identity_rejections": externalATSIdentityRejections}
	return map[string]interface{}{"meta": meta, "company_detail": detail}
}

func detectLanguage(text string) string {
	if len(text) < 50 {
		return ""
	}
	sample := text
	if len(sample) > 5000 {
		sample = sample[:5000]
	}
	total, cyrillic, cjk, arabic := 0, 0, 0, 0
	for _, r := range sample {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= 0x0400 && r <= 0x04ff) || (r >= 0x4e00 && r <= 0x9fff) || (r >= 0x0600 && r <= 0x06ff) {
			total++
		}
		if r >= 0x0400 && r <= 0x04ff {
			cyrillic++
		}
		if r >= 0x4e00 && r <= 0x9fff {
			cjk++
		}
		if r >= 0x0600 && r <= 0x06ff {
			arabic++
		}
	}
	if total == 0 {
		return ""
	}
	if float64(cjk)/float64(total) > .10 {
		return "ZH"
	}
	if float64(cyrillic)/float64(total) > .10 {
		return "RU"
	}
	if float64(arabic)/float64(total) > .10 {
		return "AR"
	}
	return "EN"
}

func detectActive(raw string, page Page) bool {
	lower := strings.ToLower(raw)
	for _, marker := range []string{"domain for sale", "this domain may be for sale", "buy this domain", "domain is for sale", "domain name for sale", "available for sale", "domain parking", "domain is parked", "this domain is parked", "parked by", "parked free", "sedo.com", "afternic.com"} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	if strings.Contains(lower, "404 not found") && len(raw) < 1500 {
		return false
	}
	if strings.Contains(lower, "403 forbidden") && len(raw) < 200 {
		return false
	}
	return len(raw) > 500 || len(page.Anchors) > 0 || len(page.Emails) > 0 || len(page.Images) > 0
}

func detectHosting(rawBody, finalURL string) bool {
	host := strings.ToLower(strings.TrimSuffix(hostname(finalURL), "."))
	if parsed := parseURL(finalURL); parsed != nil && hostingPathRE.MatchString(parsed.Path) {
		return true
	}
	if strings.HasPrefix(host, "ww") {
		parts := strings.SplitN(host, ".", 2)
		if len(parts) == 2 && len(parts[0]) > 2 {
			digits := true
			for _, r := range parts[0][2:] {
				if r < '0' || r > '9' {
					digits = false
					break
				}
			}
			if digits {
				return true
			}
		}
	}
	bare := strings.TrimPrefix(host, "www.")
	if domainBrokerHosts[bare] {
		return true
	}
	for broker := range domainBrokerHosts {
		if strings.HasSuffix(bare, "."+broker) {
			return true
		}
	}

	lower := strings.ToLower(html.UnescapeString(rawBody))
	for _, marker := range []string{
		"domain for sale", "this domain may be for sale", "buy this domain", "domain is for sale",
		"domain name for sale", "godaddy auctions", "sedo.com", "afternic.com", "available for sale",
		"domain parking", "domain is parked", "this domain is parked", "this web page is parked",
		"parked by", "parked free", "domain has expired", "this domain has expired", "renew this domain",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return landerRedirectRE.MatchString(lower) || landerReplaceRE.MatchString(lower) ||
		(strings.Contains(lower, "godaddy") && (strings.Contains(lower, "lander") || strings.Contains(lower, "parking") || strings.Contains(lower, "expired") || strings.Contains(lower, "renew")))
}

// detectWebsitePlatforms identifies common website builders from high-signal
// HTML and URL markers. It intentionally returns only platform names, keeping
// the extractor output compact while leaving the full HTML out of metadata.
func detectWebsitePlatforms(rawBody string) (string, []string) {
	// Match the Python detector against the HTML body only. Including the final
	// URL makes redirects or asset URLs look like platform proof.
	evidence := strings.ToLower(html.UnescapeString(rawBody))
	scores := map[string]int{}
	add := func(platform string, points int, markers ...string) {
		for _, marker := range markers {
			if strings.Contains(evidence, marker) {
				scores[platform] += points
				return
			}
		}
	}
	addAll := func(platform string, points int, markers ...string) {
		for _, marker := range markers {
			if !strings.Contains(evidence, marker) {
				return
			}
		}
		scores[platform] += points
	}

	add("wordpress", 6, "wp-content", "wp-includes")
	addAll("wordpress", 6, "<meta", "generator", "wordpress")
	add("wordpress", 5, "wp-json", "rest_route", "api.w.org")
	add("wordpress", 5, "wp-json/oembed", "oembed/1.0", "wp-embed.js")
	add("wordpress", 5, "wp-emoji", "_wpemojisettings")
	add("wordpress", 4, "admin-ajax.php", "ajaxurl")
	add("wordpress", 3, "wp-block-", "wp-image-", "wp-post-image")
	add("wordpress", 3, "wpapisettings", "wp_var", "wpdata", "wp.hooks", "wp.i18n")

	add("shopify", 7, "cdn.shopify.com", "/cdn/shop", "/cdn/shopifycloud")
	add("shopify", 6, ".myshopify.com", "shopify.", "shopifyanalytics", "shopifypaymentbutton")
	add("shopify", 4, "shopify-section-", "shopify-payment-button")

	add("webflow", 7, "data-wf-site", "data-wf-page")
	add("webflow", 6, "webflow.js", "webflow.css", "webflow.push")
	add("webflow", 4, "w-webflow-badge", "w-nav", "w-slider", "w-dyn-list")

	add("wix", 7, "static.parastorage.com", "wixstatic.com")
	add("wix", 7, "wix-thunderbolt", "thunderbolt", "wixcodeapi", "wixbisession")
	add("wix", 5, "_api/wix", "x-wix-request-id")

	add("squarespace", 7, "static1.squarespace.com", "static.squarespace.com", "squarespace-cdn.com")
	add("squarespace", 5, "squarespace_context", "squarespace-ui")

	add("bigcommerce", 7, "bigcommerce.com/s-", "/images/stencil", "/stencil/assets")
	add("bigcommerce", 6, "bcdata", "stencil-utils", "bigcommerce", "bc_channel_id")

	add("hubspot", 7, "hs_cos_wrapper", "data-hs-cos-")
	add("hubspot", 6, "hs-scripts.com", "hsforms.com", "hbspt.forms.create", "_hsq", "hs-analytics")

	if dudaAssetRE.MatchString(evidence) {
		scores["duda"] += 7
	}
	if dudaAPIRE.MatchString(evidence) {
		scores["duda"] += 7
	}
	if dudaRuntimeRE.MatchString(evidence) {
		scores["duda"] += 6
	}

	add("gohighlevel", 7, "sites.ludicrous.cloud")
	add("gohighlevel", 7, "leadconnectorhq.com", "msgsndr.com", "gohighlevel.com", "highlevel.com")
	add("gohighlevel", 5, "leadconnector", "data-leadconnector", "data-ghl")

	order := []string{"wordpress", "shopify", "webflow", "wix", "squarespace", "bigcommerce", "hubspot", "duda", "gohighlevel"}
	orderIndex := map[string]int{}
	for index, platform := range order {
		orderIndex[platform] = index
	}
	detected := []string{}
	for _, platform := range order {
		if scores[platform] >= 5 {
			detected = append(detected, platform)
		}
	}
	sort.SliceStable(detected, func(i, j int) bool {
		if scores[detected[i]] != scores[detected[j]] {
			return scores[detected[i]] > scores[detected[j]]
		}
		return orderIndex[detected[i]] < orderIndex[detected[j]]
	})
	if len(detected) == 0 {
		return "", detected
	}
	return detected[0], detected
}

func firstImage(images []Image, base string) string {
	for _, image := range images {
		if value := absoluteURL(image.Href, base); value != "" {
			return value
		}
	}
	return ""
}
func statusOrNil(value int) interface{} {
	if value == 0 {
		return nil
	}
	return value
}
func errorOrNil(err error) interface{} {
	if err == nil {
		return nil
	}
	return err.Error()
}
func firstOrNil(values []string) interface{} {
	if len(values) == 0 {
		return nil
	}
	return values[0]
}
func truncate(value string, limit int) string {
	if len(value) > limit {
		return value[:limit]
	}
	return value
}

func main() {
	timeoutSeconds, workers := 15, 10
	attempts := 1
	recursive := true
	proxyValue := os.Getenv("COMPANY_EXTRACTOR_PROXY")
	browser := false
	browserFallback := strings.EqualFold(os.Getenv("COMPANY_EXTRACTOR_BROWSER_FALLBACK"), "1") || strings.EqualFold(os.Getenv("COMPANY_EXTRACTOR_BROWSER_FALLBACK"), "true")
	browserScript := os.Getenv("COMPANY_EXTRACTOR_BROWSER_SCRIPT")
	urls := []string{}
	for _, argument := range os.Args[1:] {
		switch {
		case strings.HasPrefix(argument, "--timeout="):
			timeoutSeconds, _ = strconv.Atoi(strings.TrimPrefix(argument, "--timeout="))
		case strings.HasPrefix(argument, "--workers="):
			workers, _ = strconv.Atoi(strings.TrimPrefix(argument, "--workers="))
		case strings.HasPrefix(argument, "--attempts="):
			attempts, _ = strconv.Atoi(strings.TrimPrefix(argument, "--attempts="))
		case strings.HasPrefix(argument, "--proxy="):
			proxyValue = strings.TrimPrefix(argument, "--proxy=")
		case argument == "--browser":
			browser = true
		case argument == "--browser-fallback":
			browserFallback = true
		case strings.HasPrefix(argument, "--browser-script="):
			browserScript = strings.TrimPrefix(argument, "--browser-script=")
		case argument == "--no-recursive":
			recursive = false
		case !strings.HasPrefix(argument, "--"):
			urls = append(urls, argument)
		}
	}
	if len(urls) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: company-extractor [--timeout=SECONDS] [--workers=N] [--attempts=N] [--no-recursive] URL ...")
		os.Exit(1)
	}
	if workers < 1 {
		workers = 1
	}
	if timeoutSeconds < 1 {
		timeoutSeconds = 1
	}
	if attempts < 1 {
		attempts = 1
	}
	var proxy *url.URL
	if proxyValue != "" && !strings.EqualFold(proxyValue, "none") && !strings.EqualFold(proxyValue, "direct") {
		parsed, err := url.Parse(proxyValue)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			fmt.Fprintf(os.Stderr, "invalid proxy URL: %s\n", proxyValue)
			os.Exit(2)
		}
		proxy = parsed
	}
	if browser || browserFallback {
		fetchConfig.BrowserScript = browserScript
		if fetchConfig.BrowserScript == "" {
			fetchConfig.BrowserScript = "/Users/macitsimsek/code/sponsor-companies/scripts/browser_fetch.mjs"
		}
	}
	fetchConfig.Proxy, fetchConfig.Browser, fetchConfig.Attempts = proxy, browser, attempts
	inFlight := workers
	if inFlight > 128 {
		inFlight = 128
	}
	if inFlight < 1 {
		inFlight = 1
	}
	httpSlots = make(chan struct{}, inFlight)
	results := make([]map[string]interface{}, len(urls))
	jobs := make(chan int)
	var wait sync.WaitGroup
	for worker := 0; worker < workers && worker < len(urls); worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range jobs {
				results[index] = extractOne(urls[index], time.Duration(timeoutSeconds)*time.Second, recursive)
			}
		}()
	}
	for index := range urls {
		jobs <- index
	}
	close(jobs)
	wait.Wait()
	// The batch runner consumes this output as machine-readable JSON. Compact
	// encoding avoids generating and reparsing a large amount of whitespace
	// without changing the result schema or extracted values.
	encoded, _ := json.Marshal(results)
	fmt.Println(string(encoded))
}
