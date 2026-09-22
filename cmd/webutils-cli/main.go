package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/groovy-sky/groovy-agent/webutils"
)

const (
	exitCodeSuccess = 0
	exitCodeError   = 1
	exitCodeUsage   = 2

	screenshotModeViewport = "viewport"
	screenshotModeFullPage = "full_page"
	searchEngineDuckDuckGo = "duckduckgo"
)

type browserFactory func(webutils.Limits) webutils.Browser

type browseOptions struct {
	url            string
	maxTextChars   int
	screenshot     bool
	screenshotMode string
	screenshotOut  string
	actions        browserActionsFlag
	timeout        time.Duration
}

type searchOptions struct {
	query      string
	maxResults int
	engine     string
	timeout    time.Duration
}

type browserActionsFlag []webutils.BrowserAction

type browseOutput struct {
	FinalURL            string          `json:"final_url"`
	Title               string          `json:"title"`
	Content             string          `json:"content"`
	ContentFormat       string          `json:"content_format"`
	ExtractionMethod    string          `json:"extraction_method"`
	VisibleText         string          `json:"visible_text"`
	Links               []webutils.Link `json:"links"`
	Truncated           bool            `json:"truncated"`
	ScreenshotPNGBase64 string          `json:"screenshot_png_base64,omitempty"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(runCLI(ctx, os.Args[1:], os.Stdout, os.Stderr, func(limits webutils.Limits) webutils.Browser {
		return webutils.NewChromiumBrowser(limits)
	}))
}

func runCLI(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer, newBrowser browserFactory) int {
	if len(args) == 0 {
		printRootUsage(stderr)
		return exitCodeUsage
	}
	if isHelpArg(args[0]) || args[0] == "help" {
		printRootUsage(stderr)
		return exitCodeSuccess
	}

	switch args[0] {
	case "browse":
		return runBrowse(ctx, args[1:], stdout, stderr, newBrowser)
	case "search":
		return runSearch(ctx, args[1:], stdout, stderr, newBrowser)
	default:
		fmt.Fprintf(stderr, "webutils-cli: unknown subcommand %q\n\n", args[0])
		printRootUsage(stderr)
		return exitCodeUsage
	}
}

func runBrowse(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer, newBrowser browserFactory) int {
	limits := webutils.DefaultLimits()
	options := browseOptions{
		maxTextChars:   limits.DefaultMaxTextChars,
		screenshotMode: screenshotModeViewport,
	}
	flags := newBrowseFlagSet(stderr, &options)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitCodeSuccess
		}
		return exitCodeUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "webutils-cli browse: unexpected arguments: %s\n", strings.Join(flags.Args(), " "))
		flags.Usage()
		return exitCodeUsage
	}
	if strings.TrimSpace(options.url) == "" {
		fmt.Fprintln(stderr, "webutils-cli browse: -url is required")
		flags.Usage()
		return exitCodeUsage
	}
	if options.screenshotOut != "" && !options.screenshot {
		fmt.Fprintln(stderr, "webutils-cli browse: -screenshot-out requires -screenshot")
		flags.Usage()
		return exitCodeUsage
	}

	req, effectiveLimits, err := buildBrowseRequest(options, limits)
	if err != nil {
		fmt.Fprintf(stderr, "webutils-cli browse: %s\n", err)
		flags.Usage()
		return exitCodeUsage
	}

	result, err := newBrowser(effectiveLimits).Browse(ctx, req)
	if err != nil {
		fmt.Fprintf(stderr, "webutils-cli: %s\n", err)
		return exitCodeError
	}
	if err := writeBrowseResult(stdout, result, options.screenshotOut); err != nil {
		fmt.Fprintf(stderr, "webutils-cli: %s\n", err)
		return exitCodeError
	}
	return exitCodeSuccess
}

func runSearch(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer, newBrowser browserFactory) int {
	limits := webutils.DefaultLimits()
	options := searchOptions{
		maxResults: limits.DefaultMaxResults,
		engine:     searchEngineDuckDuckGo,
	}
	flags := newSearchFlagSet(stderr, &options)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitCodeSuccess
		}
		return exitCodeUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "webutils-cli search: unexpected arguments: %s\n", strings.Join(flags.Args(), " "))
		flags.Usage()
		return exitCodeUsage
	}
	if strings.TrimSpace(options.query) == "" {
		fmt.Fprintln(stderr, "webutils-cli search: -query is required")
		flags.Usage()
		return exitCodeUsage
	}

	req, effectiveLimits, err := buildSearchRequest(options, limits)
	if err != nil {
		fmt.Fprintf(stderr, "webutils-cli search: %s\n", err)
		flags.Usage()
		return exitCodeUsage
	}

	result, err := newBrowser(effectiveLimits).Search(ctx, req)
	if err != nil {
		fmt.Fprintf(stderr, "webutils-cli: %s\n", err)
		return exitCodeError
	}
	if err := writeJSON(stdout, result); err != nil {
		fmt.Fprintf(stderr, "webutils-cli: %s\n", err)
		return exitCodeError
	}
	return exitCodeSuccess
}

func buildBrowseRequest(options browseOptions, limits webutils.Limits) (webutils.BrowseRequest, webutils.Limits, error) {
	effectiveLimits := limits
	if options.timeout > 0 {
		effectiveLimits.Timeout = options.timeout
	}

	screenshotMode, err := normalizeScreenshotMode(options.screenshotMode)
	if err != nil {
		return webutils.BrowseRequest{}, limits, err
	}

	return webutils.BrowseRequest{
		URL:               strings.TrimSpace(options.url),
		MaxTextChars:      clampPositive(options.maxTextChars, limits.DefaultMaxTextChars, limits.MaxAllowedTextChars),
		CaptureScreenshot: options.screenshot,
		ScreenshotMode:    screenshotMode,
		Actions:           append([]webutils.BrowserAction(nil), options.actions...),
	}, effectiveLimits, nil
}

func buildSearchRequest(options searchOptions, limits webutils.Limits) (webutils.SearchRequest, webutils.Limits, error) {
	effectiveLimits := limits
	if options.timeout > 0 {
		effectiveLimits.Timeout = options.timeout
	}

	engine, err := normalizeSearchEngine(options.engine)
	if err != nil {
		return webutils.SearchRequest{}, limits, err
	}

	return webutils.SearchRequest{
		Query:      strings.TrimSpace(options.query),
		MaxResults: clampPositive(options.maxResults, limits.DefaultMaxResults, limits.MaxSearchResults),
		Engine:     engine,
	}, effectiveLimits, nil
}

func writeBrowseResult(stdout io.Writer, result webutils.BrowseResult, screenshotOut string) error {
	output := browseOutput{
		FinalURL:         result.FinalURL,
		Title:            result.Title,
		Content:          result.Content,
		ContentFormat:    result.ContentFormat,
		ExtractionMethod: result.ExtractionMethod,
		VisibleText:      result.VisibleText,
		Links:            result.Links,
		Truncated:        result.Truncated,
	}
	if len(result.ScreenshotPNG) > 0 {
		if screenshotOut != "" {
			if err := os.WriteFile(screenshotOut, result.ScreenshotPNG, 0o644); err != nil {
				return fmt.Errorf("write screenshot: %w", err)
			}
		} else {
			output.ScreenshotPNGBase64 = base64.StdEncoding.EncodeToString(result.ScreenshotPNG)
		}
	}
	return writeJSON(stdout, output)
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func newBrowseFlagSet(output io.Writer, options *browseOptions) *flag.FlagSet {
	flags := flag.NewFlagSet("browse", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&options.url, "url", "", "public HTTPS URL to browse")
	flags.IntVar(&options.maxTextChars, "max-text-chars", options.maxTextChars, "maximum characters returned for extracted content and visible text")
	flags.BoolVar(&options.screenshot, "screenshot", false, "capture a PNG screenshot")
	flags.StringVar(&options.screenshotMode, "screenshot-mode", options.screenshotMode, "screenshot mode: viewport or full_page")
	flags.StringVar(&options.screenshotOut, "screenshot-out", "", "write PNG screenshot bytes to this path instead of embedding base64 in JSON")
	flags.Var(&options.actions, "action", `browser action JSON object; repeat to execute multiple actions in order, e.g. -action '{"type":"click","selector":"#submit"}'`)
	flags.DurationVar(&options.timeout, "timeout", 0, "override browser timeout (for example 10s or 1m)")
	flags.Usage = func() {
		fmt.Fprintf(output, "Usage: webutils-cli browse [flags]\n\nBrowse one page and emit JSON.\n\nFlags:\n")
		flags.PrintDefaults()
	}
	return flags
}

func newSearchFlagSet(output io.Writer, options *searchOptions) *flag.FlagSet {
	flags := flag.NewFlagSet("search", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&options.query, "query", "", "search query text")
	flags.IntVar(&options.maxResults, "max-results", options.maxResults, "maximum number of search results returned")
	flags.StringVar(&options.engine, "engine", options.engine, "search engine identifier")
	flags.DurationVar(&options.timeout, "timeout", 0, "override browser timeout (for example 10s or 1m)")
	flags.Usage = func() {
		fmt.Fprintf(output, "Usage: webutils-cli search [flags]\n\nSearch the public web and emit JSON.\n\nFlags:\n")
		flags.PrintDefaults()
	}
	return flags
}

func printRootUsage(output io.Writer) {
	browseOptions := browseOptions{
		maxTextChars:   webutils.DefaultLimits().DefaultMaxTextChars,
		screenshotMode: screenshotModeViewport,
	}
	searchOptions := searchOptions{
		maxResults: webutils.DefaultLimits().DefaultMaxResults,
		engine:     searchEngineDuckDuckGo,
	}

	fmt.Fprintln(output, "Usage: webutils-cli <subcommand> [flags]")
	fmt.Fprintln(output)
	fmt.Fprintln(output, "Subcommands:")
	fmt.Fprintln(output, "  browse   Browse one page with Chromium and emit JSON")
	fmt.Fprintln(output, "  search   Search the public web with Chromium and emit JSON")
	fmt.Fprintln(output)
	newBrowseFlagSet(output, &browseOptions).Usage()
	fmt.Fprintln(output)
	newSearchFlagSet(output, &searchOptions).Usage()
}

func isHelpArg(arg string) bool {
	return arg == "-h" || arg == "-help" || arg == "--help"
}

func (f *browserActionsFlag) String() string {
	if len(*f) == 0 {
		return ""
	}
	encoded, err := json.Marshal([]webutils.BrowserAction(*f))
	if err != nil {
		return ""
	}
	return string(encoded)
}

func (f *browserActionsFlag) Set(value string) error {
	action, err := parseBrowserAction(value)
	if err != nil {
		return err
	}
	*f = append(*f, action)
	return nil
}

func parseBrowserAction(value string) (webutils.BrowserAction, error) {
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()

	var action webutils.BrowserAction
	if err := decoder.Decode(&action); err != nil {
		return webutils.BrowserAction{}, fmt.Errorf("invalid -action JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return webutils.BrowserAction{}, errors.New("invalid -action JSON: must contain exactly one object")
	}

	action.Type = strings.TrimSpace(action.Type)
	action.Selector = strings.TrimSpace(action.Selector)
	action.Value = strings.TrimSpace(action.Value)
	switch action.Type {
	case "wait_visible", "hover", "click":
		if action.Selector == "" {
			return webutils.BrowserAction{}, errors.New(`invalid -action JSON: "selector" is required`)
		}
		if action.Value != "" {
			return webutils.BrowserAction{}, fmt.Errorf("invalid -action JSON: action type %q does not accept a value", action.Type)
		}
	case "set_value", "type":
		if action.Selector == "" {
			return webutils.BrowserAction{}, errors.New(`invalid -action JSON: "selector" is required`)
		}
		if action.Value == "" {
			return webutils.BrowserAction{}, fmt.Errorf("invalid -action JSON: action type %q requires a value", action.Type)
		}
	default:
		return webutils.BrowserAction{}, fmt.Errorf("invalid -action JSON: unsupported action type %q", action.Type)
	}

	return action, nil
}

func normalizeScreenshotMode(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", screenshotModeViewport:
		return screenshotModeViewport, nil
	case screenshotModeFullPage:
		return screenshotModeFullPage, nil
	default:
		return "", fmt.Errorf("unsupported screenshot mode %q", mode)
	}
}

func normalizeSearchEngine(engine string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(engine)) {
	case "", searchEngineDuckDuckGo:
		return searchEngineDuckDuckGo, nil
	default:
		return "", fmt.Errorf("unsupported search engine %q", engine)
	}
}

func clampPositive(value int, fallback int, max int) int {
	if value <= 0 {
		value = fallback
	}
	if max > 0 && value > max {
		return max
	}
	return value
}
