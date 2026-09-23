package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/groovy-sky/groovy-agent/webutils"
)

type fakeBrowser struct {
	browseResult webutils.BrowseResult
	browseErr    error
	searchResult webutils.SearchResult
	searchErr    error
	lastBrowse   webutils.BrowseRequest
	lastSearch   webutils.SearchRequest
}

func (f *fakeBrowser) Browse(_ context.Context, req webutils.BrowseRequest) (webutils.BrowseResult, error) {
	f.lastBrowse = req
	return f.browseResult, f.browseErr
}

func (f *fakeBrowser) Search(_ context.Context, req webutils.SearchRequest) (webutils.SearchResult, error) {
	f.lastSearch = req
	return f.searchResult, f.searchErr
}

func TestParseBrowserActionValid(t *testing.T) {
	t.Parallel()

	action, err := parseBrowserAction(`{"type":"click","selector":"#submit"}`)
	if err != nil {
		t.Fatalf("parseBrowserAction returned error: %v", err)
	}
	if action.Type != "click" || action.Selector != "#submit" || action.Value != "" {
		t.Fatalf("unexpected action: %+v", action)
	}
}

func TestParseBrowserActionPreservesValueWhitespace(t *testing.T) {
	t.Parallel()

	action, err := parseBrowserAction(`{"type":"type","selector":"#target-input","value":" plus more "}`)
	if err != nil {
		t.Fatalf("parseBrowserAction returned error: %v", err)
	}
	if action.Value != " plus more " {
		t.Fatalf("expected value whitespace to be preserved, got %q", action.Value)
	}
}

func TestParseBrowserActionInvalid(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		value string
		want  string
	}{
		{
			name:  "invalid json",
			value: `{"type":"click"`,
			want:  "invalid -action JSON",
		},
		{
			name:  "unknown field",
			value: `{"type":"click","selector":"#submit","extra":true}`,
			want:  "invalid -action JSON",
		},
		{
			name:  "unsupported type",
			value: `{"type":"submit","selector":"#submit"}`,
			want:  `unsupported action type "submit"`,
		},
		{
			name:  "missing value",
			value: `{"type":"type","selector":"input[name=q]"}`,
			want:  `action type "type" requires a value`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseBrowserAction(tc.value)
			if err == nil {
				t.Fatal("expected parseBrowserAction to fail")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %q", tc.want, err)
			}
		})
	}
}

func TestBuildBrowseRequestDefaultsAndClamps(t *testing.T) {
	t.Parallel()

	limits := webutils.DefaultLimits()
	options := browseOptions{
		url:            " https://example.com ",
		maxTextChars:   0,
		screenshot:     true,
		screenshotMode: "",
		actions: browserActionsFlag{
			{Type: "click", Selector: "#submit"},
		},
		timeout: 45 * time.Second,
	}

	req, effectiveLimits, err := buildBrowseRequest(options, limits)
	if err != nil {
		t.Fatalf("buildBrowseRequest returned error: %v", err)
	}
	if req.URL != "https://example.com" {
		t.Fatalf("expected trimmed URL, got %q", req.URL)
	}
	if req.MaxTextChars != limits.DefaultMaxTextChars {
		t.Fatalf("expected default max text chars %d, got %d", limits.DefaultMaxTextChars, req.MaxTextChars)
	}
	if req.ScreenshotMode != screenshotModeViewport {
		t.Fatalf("expected default screenshot mode %q, got %q", screenshotModeViewport, req.ScreenshotMode)
	}
	if effectiveLimits.Timeout != 45*time.Second {
		t.Fatalf("expected timeout override, got %s", effectiveLimits.Timeout)
	}
}

func TestBuildBrowseRequestClampsToMax(t *testing.T) {
	t.Parallel()

	limits := webutils.DefaultLimits()
	req, _, err := buildBrowseRequest(browseOptions{
		url:          "https://example.com",
		maxTextChars: limits.MaxAllowedTextChars + 100,
	}, limits)
	if err != nil {
		t.Fatalf("buildBrowseRequest returned error: %v", err)
	}
	if req.MaxTextChars != limits.MaxAllowedTextChars {
		t.Fatalf("expected clamped max text chars %d, got %d", limits.MaxAllowedTextChars, req.MaxTextChars)
	}
}

func TestBuildSearchRequestDefaultsAndClamps(t *testing.T) {
	t.Parallel()

	limits := webutils.DefaultLimits()
	options := searchOptions{
		query:      " golang ",
		maxResults: 0,
		engine:     "",
		timeout:    15 * time.Second,
	}

	req, effectiveLimits, err := buildSearchRequest(options, limits)
	if err != nil {
		t.Fatalf("buildSearchRequest returned error: %v", err)
	}
	if req.Query != "golang" {
		t.Fatalf("expected trimmed query, got %q", req.Query)
	}
	if req.MaxResults != limits.DefaultMaxResults {
		t.Fatalf("expected default max results %d, got %d", limits.DefaultMaxResults, req.MaxResults)
	}
	if req.Engine != searchEngineDuckDuckGo {
		t.Fatalf("expected default search engine %q, got %q", searchEngineDuckDuckGo, req.Engine)
	}
	if effectiveLimits.Timeout != 15*time.Second {
		t.Fatalf("expected timeout override, got %s", effectiveLimits.Timeout)
	}
}

func TestBuildSearchRequestClampsToMax(t *testing.T) {
	t.Parallel()

	limits := webutils.DefaultLimits()
	req, _, err := buildSearchRequest(searchOptions{
		query:      "golang",
		maxResults: limits.MaxSearchResults + 10,
	}, limits)
	if err != nil {
		t.Fatalf("buildSearchRequest returned error: %v", err)
	}
	if req.MaxResults != limits.MaxSearchResults {
		t.Fatalf("expected clamped max results %d, got %d", limits.MaxSearchResults, req.MaxResults)
	}
}

func TestRunCLIUsageErrors(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing subcommand", args: nil, want: "Usage: webutils-cli <subcommand> [flags]"},
		{name: "unknown subcommand", args: []string{"unknown"}, want: `unknown subcommand "unknown"`},
		{name: "browse missing url", args: []string{"browse"}, want: "-url is required"},
		{name: "search missing query", args: []string{"search"}, want: "-query is required"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			factoryCalled := false
			exitCode := runCLI(context.Background(), tc.args, &stdout, &stderr, func(limits webutils.Limits) webutils.Browser {
				factoryCalled = true
				return &fakeBrowser{}
			})
			if exitCode != exitCodeUsage {
				t.Fatalf("expected usage exit code %d, got %d", exitCodeUsage, exitCode)
			}
			if factoryCalled {
				t.Fatal("expected browser factory not to be called on usage error")
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("expected stderr to contain %q, got %q", tc.want, stderr.String())
			}
		})
	}
}

func TestRunBrowseWritesEmbeddedScreenshotJSON(t *testing.T) {
	t.Parallel()

	browser := &fakeBrowser{
		browseResult: webutils.BrowseResult{
			FinalURL:         "https://example.com",
			Title:            "Example",
			Content:          "hello",
			ContentFormat:    "markdown",
			ExtractionMethod: "readability_markdown",
			VisibleText:      "hello",
			Links:            []webutils.Link{{Text: "About", URL: "https://example.com/about"}},
			ScreenshotPNG:    []byte("png-data"),
		},
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runCLI(context.Background(), []string{"browse", "-url", "https://example.com", "-screenshot"}, &stdout, &stderr, func(limits webutils.Limits) webutils.Browser {
		return browser
	})
	if exitCode != exitCodeSuccess {
		t.Fatalf("expected success exit code, got %d with stderr %q", exitCode, stderr.String())
	}

	var body map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &body); err != nil {
		t.Fatalf("decode browse output: %v", err)
	}
	if body["final_url"] != "https://example.com" {
		t.Fatalf("unexpected browse output: %+v", body)
	}
	if body["screenshot_png_base64"] != base64.StdEncoding.EncodeToString([]byte("png-data")) {
		t.Fatalf("expected embedded screenshot, got %+v", body)
	}
	if browser.lastBrowse.URL != "https://example.com" {
		t.Fatalf("expected browse request to be forwarded, got %+v", browser.lastBrowse)
	}
}

func TestRunSearchDispatchesToBrowser(t *testing.T) {
	t.Parallel()

	browser := &fakeBrowser{
		searchResult: webutils.SearchResult{
			Engine:   searchEngineDuckDuckGo,
			Query:    "golang",
			FinalURL: "https://duckduckgo.com/html/?q=golang",
			Results:  []webutils.Link{{Text: "Go", URL: "https://go.dev", Rank: 1}},
		},
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runCLI(context.Background(), []string{"search", "-query", "golang"}, &stdout, &stderr, func(limits webutils.Limits) webutils.Browser {
		return browser
	})
	if exitCode != exitCodeSuccess {
		t.Fatalf("expected success exit code, got %d with stderr %q", exitCode, stderr.String())
	}
	if browser.lastSearch.Query != "golang" || browser.lastSearch.Engine != searchEngineDuckDuckGo {
		t.Fatalf("expected search request to be forwarded, got %+v", browser.lastSearch)
	}
}

func TestRunBrowseWritesScreenshotFile(t *testing.T) {
	t.Parallel()

	browser := &fakeBrowser{
		browseResult: webutils.BrowseResult{
			FinalURL:      "https://example.com",
			ScreenshotPNG: []byte("png-data"),
		},
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	screenshotPath := t.TempDir() + "/shot.png"

	exitCode := runCLI(context.Background(), []string{"browse", "-url", "https://example.com", "-screenshot", "-screenshot-out", screenshotPath}, &stdout, &stderr, func(limits webutils.Limits) webutils.Browser {
		return browser
	})
	if exitCode != exitCodeSuccess {
		t.Fatalf("expected success exit code, got %d with stderr %q", exitCode, stderr.String())
	}

	var body map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &body); err != nil {
		t.Fatalf("decode browse output: %v", err)
	}
	if _, exists := body["screenshot_png_base64"]; exists {
		t.Fatalf("expected screenshot base64 to be omitted when writing file, got %+v", body)
	}
	data, err := os.ReadFile(screenshotPath)
	if err != nil {
		t.Fatalf("read screenshot file: %v", err)
	}
	if string(data) != "png-data" {
		t.Fatalf("unexpected screenshot file contents %q", data)
	}
}

func TestRunCommandReportsBrowserErrors(t *testing.T) {
	t.Parallel()

	browser := &fakeBrowser{searchErr: errors.New("boom")}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runCLI(context.Background(), []string{"search", "-query", "golang"}, &stdout, &stderr, func(limits webutils.Limits) webutils.Browser {
		return browser
	})
	if exitCode != exitCodeError {
		t.Fatalf("expected error exit code %d, got %d", exitCodeError, exitCode)
	}
	if !strings.Contains(stderr.String(), "webutils-cli: boom") {
		t.Fatalf("expected stderr to contain execution error, got %q", stderr.String())
	}
}
