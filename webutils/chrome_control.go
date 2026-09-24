package webutils

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/groovy-sky/groovy-agent/internal/jsonschema"
	"github.com/groovy-sky/groovy-agent/internal/mcpproto"
)

const (
	chromeControlURLEnvVar       = "WEBUTILS_CHROME_CONTROL_URL"
	chromeControlNoVNCURLEnvVar  = "WEBUTILS_CHROME_CONTROL_NOVNC_URL"
	defaultChromeControlTimeout  = 20 * time.Second
	maxChromeControlResponseBody = 1 << 20
	sessionTokenMinLength        = 8
	sessionTokenMaxLength        = 128
	sessionPollDefaultSeconds    = 30
	sessionPollMaxSeconds        = 120
)

var sessionTokenRegexp = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]{7,127}$`)

type chromeControlClient struct {
	baseURL  *url.URL
	noVNCURL string
	client   *http.Client
	resolver policyResolver
}

type chromeControlSessionCreateRequest struct {
	URL               string `json:"url,omitempty"`
	CaptureScreenshot bool   `json:"capture_screenshot,omitempty"`
	MaxTextChars      int    `json:"max_text_chars,omitempty"`
}

type chromeControlSessionCreateResponse struct {
	Token  string `json:"token"`
	Status string `json:"status"`
}

type browserSessionToolError struct {
	category string
	message  string
}

func (e browserSessionToolError) Error() string {
	return e.message
}

func newChromeControlClientFromEnv(resolver policyResolver) (*chromeControlClient, error) {
	raw := strings.TrimSpace(os.Getenv(chromeControlURLEnvVar))
	if raw == "" {
		return nil, nil
	}
	baseURL, err := parseHTTPBaseURL(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", chromeControlURLEnvVar, err)
	}
	noVNC := strings.TrimSpace(os.Getenv(chromeControlNoVNCURLEnvVar))
	if noVNC != "" {
		if _, err := parseHTTPBaseURL(noVNC); err != nil {
			return nil, fmt.Errorf("%s: %w", chromeControlNoVNCURLEnvVar, err)
		}
	}
	if resolver == nil {
		resolver = defaultResolver()
	}
	return &chromeControlClient{
		baseURL:  baseURL,
		noVNCURL: strings.TrimRight(noVNC, "/"),
		client:   &http.Client{Timeout: defaultChromeControlTimeout},
		resolver: resolver,
	}, nil
}

func parseHTTPBaseURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("must be a valid URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("must use http or https")
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("host is required")
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("userinfo is not allowed")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("query and fragment are not allowed")
	}
	return parsed, nil
}

func inputSchemaBrowserSessionCreate() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "Optional public HTTPS URL to open in the remote interactive browser session.",
				"maxLength":   maxURLLength,
			},
			"capture_screenshot": map[string]any{
				"type":        "boolean",
				"description": "Whether remote extraction should include screenshot metadata when supported by the backend.",
				"default":     false,
			},
			"max_text_chars": map[string]any{
				"type":        "integer",
				"description": "Maximum extracted content length requested from the remote backend.",
				"minimum":     1,
				"maximum":     maxAllowedTextChars,
			},
		},
		"additionalProperties": false,
	}
}

func inputSchemaBrowserSessionTokenOnly() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"token": map[string]any{
				"type":        "string",
				"description": "Opaque interactive browser session token.",
				"minLength":   sessionTokenMinLength,
				"maxLength":   sessionTokenMaxLength,
			},
		},
		"required":             []any{"token"},
		"additionalProperties": false,
	}
}

func inputSchemaBrowserSessionContinue() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"token": map[string]any{
				"type":        "string",
				"description": "Opaque interactive browser session token.",
				"minLength":   sessionTokenMinLength,
				"maxLength":   sessionTokenMaxLength,
			},
			"wait_for_completion": map[string]any{
				"type":        "boolean",
				"description": "If true (default), poll the backend until the session reaches a terminal status or timeout.",
				"default":     true,
			},
			"max_wait_seconds": map[string]any{
				"type":        "integer",
				"description": "Maximum seconds to poll for completion when wait_for_completion is true.",
				"minimum":     1,
				"maximum":     sessionPollMaxSeconds,
			},
		},
		"required":             []any{"token"},
		"additionalProperties": false,
	}
}

func (s *Server) callBrowserSessionTool(ctx context.Context, toolName string, raw json.RawMessage) mcpproto.CallToolResult {
	switch toolName {
	case toolNameBrowserSessionCreate:
		arguments, err := jsonschema.ValidateRaw(inputSchemaBrowserSessionCreate(), raw)
		if err != nil {
			return errorResult(mcpproto.ErrorInvalidArguments, err.Error())
		}
		req := chromeControlSessionCreateRequest{
			URL:               optionalString(arguments, "url"),
			CaptureScreenshot: optionalBool(arguments, "capture_screenshot"),
			MaxTextChars:      optionalInt(arguments, "max_text_chars"),
		}
		if strings.TrimSpace(req.URL) != "" {
			if _, err := validateAndResolveURL(ctx, s.chromeControl.resolver, req.URL); err != nil {
				category, message := classifyError(err)
				return errorResult(category, message)
			}
		}
		result, err := s.chromeControl.createSession(ctx, req)
		if err != nil {
			return mapBrowserSessionError(err)
		}
		s.rememberSessionToken(result.Token)
		body := map[string]any{
			"success":   true,
			"token":     result.Token,
			"status":    result.Status,
			"novnc_url": s.chromeControl.noVNCURLForHuman(),
		}
		return mcpproto.CallToolResult{Content: []mcpproto.Content{{Type: "text", Text: encode(body)}}}
	case toolNameBrowserSessionStatus:
		arguments, err := jsonschema.ValidateRaw(inputSchemaBrowserSessionTokenOnly(), raw)
		if err != nil {
			return errorResult(mcpproto.ErrorInvalidArguments, err.Error())
		}
		token := optionalString(arguments, "token")
		if err := validateSessionToken(token); err != nil {
			return errorResult(mcpproto.ErrorInvalidArguments, err.Error())
		}
		status, err := s.chromeControl.sessionStatus(ctx, token)
		if err != nil {
			return mapBrowserSessionError(err)
		}
		if isTerminalSessionStatus(status) {
			s.forgetSessionToken(token)
		}
		return mcpproto.CallToolResult{Content: []mcpproto.Content{{Type: "text", Text: encode(map[string]any{
			"success": true,
			"token":   token,
			"session": status,
		})}}}
	case toolNameBrowserSessionContinue:
		arguments, err := jsonschema.ValidateRaw(inputSchemaBrowserSessionContinue(), raw)
		if err != nil {
			return errorResult(mcpproto.ErrorInvalidArguments, err.Error())
		}
		token := optionalString(arguments, "token")
		if err := validateSessionToken(token); err != nil {
			return errorResult(mcpproto.ErrorInvalidArguments, err.Error())
		}
		waitForCompletion := true
		if _, exists := arguments["wait_for_completion"]; exists {
			waitForCompletion = optionalBool(arguments, "wait_for_completion")
		}
		maxWaitSeconds := optionalInt(arguments, "max_wait_seconds")
		if maxWaitSeconds <= 0 {
			maxWaitSeconds = sessionPollDefaultSeconds
		}
		if maxWaitSeconds > sessionPollMaxSeconds {
			maxWaitSeconds = sessionPollMaxSeconds
		}
		if err := s.chromeControl.continueSession(ctx, token); err != nil {
			return mapBrowserSessionError(err)
		}
		status, err := s.chromeControl.sessionStatus(ctx, token)
		if err != nil {
			return mapBrowserSessionError(err)
		}
		if waitForCompletion {
			deadline := time.NewTimer(time.Duration(maxWaitSeconds) * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(1 * time.Second)
			defer ticker.Stop()
			for !isTerminalSessionStatus(status) {
				select {
				case <-ctx.Done():
					return errorResult(mcpproto.ErrorTimeout, "remote session polling timed out")
				case <-deadline.C:
					body := map[string]any{"success": true, "token": token, "session": status, "timed_out": true}
					return mcpproto.CallToolResult{Content: []mcpproto.Content{{Type: "text", Text: encode(body)}}}
				case <-ticker.C:
					status, err = s.chromeControl.sessionStatus(ctx, token)
					if err != nil {
						return mapBrowserSessionError(err)
					}
				}
			}
		}
		if isTerminalSessionStatus(status) {
			s.forgetSessionToken(token)
		}
		return mcpproto.CallToolResult{Content: []mcpproto.Content{{Type: "text", Text: encode(map[string]any{
			"success": true,
			"token":   token,
			"session": status,
		})}}}
	case toolNameBrowserSessionCancel:
		arguments, err := jsonschema.ValidateRaw(inputSchemaBrowserSessionTokenOnly(), raw)
		if err != nil {
			return errorResult(mcpproto.ErrorInvalidArguments, err.Error())
		}
		token := optionalString(arguments, "token")
		if err := validateSessionToken(token); err != nil {
			return errorResult(mcpproto.ErrorInvalidArguments, err.Error())
		}
		if err := s.chromeControl.cancelSession(ctx, token); err != nil {
			return mapBrowserSessionError(err)
		}
		s.forgetSessionToken(token)
		return mcpproto.CallToolResult{Content: []mcpproto.Content{{Type: "text", Text: encode(map[string]any{
			"success": true,
			"token":   token,
			"status":  "cancelled",
		})}}}
	default:
		return errorResult(mcpproto.ErrorUnknownTool, "tool is not available")
	}
}

func mapBrowserSessionError(err error) mcpproto.CallToolResult {
	category := mcpproto.ErrorToolError
	message := "interactive browser session operation failed"
	if err != nil {
		message = err.Error()
	}
	var typed browserSessionToolError
	if errors.As(err, &typed) {
		category = typed.category
		message = typed.message
	}
	return errorResult(category, message)
}

func validateSessionToken(token string) error {
	token = strings.TrimSpace(token)
	if len(token) < sessionTokenMinLength || len(token) > sessionTokenMaxLength {
		return fmt.Errorf("token length is out of bounds")
	}
	if !sessionTokenRegexp.MatchString(token) {
		return fmt.Errorf("token format is invalid")
	}
	return nil
}

func isTerminalSessionStatus(raw map[string]any) bool {
	status := strings.ToLower(strings.TrimSpace(optionalString(raw, "status")))
	switch status {
	case "completed", "failed", "cancelled", "expired":
		return true
	default:
		return false
	}
}

func (c *chromeControlClient) noVNCURLForHuman() string {
	if c == nil || c.noVNCURL == "" {
		return ""
	}
	parsed, err := url.Parse(c.noVNCURL)
	if err != nil {
		return c.noVNCURL
	}
	if parsed.Path == "" || parsed.Path == "/" {
		parsed.Path = "/vnc.html"
	}
	return parsed.String()
}

func (s *Server) rememberSessionToken(token string) {
	s.sessionMutex.Lock()
	defer s.sessionMutex.Unlock()
	if s.sessionTokens == nil {
		s.sessionTokens = map[string]struct{}{}
	}
	s.sessionTokens[token] = struct{}{}
}

func (s *Server) forgetSessionToken(token string) {
	s.sessionMutex.Lock()
	defer s.sessionMutex.Unlock()
	delete(s.sessionTokens, token)
}

func (c *chromeControlClient) createSession(ctx context.Context, payload chromeControlSessionCreateRequest) (chromeControlSessionCreateResponse, error) {
	response := chromeControlSessionCreateResponse{}
	endpoint := c.resolvePath("/v1/sessions")
	if err := c.doJSON(ctx, http.MethodPost, endpoint, payload, &response); err != nil {
		return chromeControlSessionCreateResponse{}, err
	}
	if err := validateSessionToken(response.Token); err != nil {
		return chromeControlSessionCreateResponse{}, browserSessionToolError{category: mcpproto.ErrorToolError, message: "remote browser service returned an invalid token"}
	}
	return response, nil
}

func (c *chromeControlClient) sessionStatus(ctx context.Context, token string) (map[string]any, error) {
	response := map[string]any{}
	if err := c.doJSON(ctx, http.MethodGet, c.resolvePath("/v1/sessions/"+token), nil, &response); err != nil {
		return nil, err
	}
	return response, nil
}

func (c *chromeControlClient) continueSession(ctx context.Context, token string) error {
	return c.doJSON(ctx, http.MethodPost, c.resolvePath("/v1/sessions/"+token+"/continue"), map[string]any{}, nil)
}

func (c *chromeControlClient) cancelSession(ctx context.Context, token string) error {
	return c.doJSON(ctx, http.MethodDelete, c.resolvePath("/v1/sessions/"+token), nil, nil)
}

func (c *chromeControlClient) resolvePath(suffix string) string {
	resolved := *c.baseURL
	basePath := strings.TrimSuffix(resolved.Path, "/")
	resolved.Path = path.Clean(basePath + "/" + strings.TrimPrefix(suffix, "/"))
	return resolved.String()
}

func (c *chromeControlClient) doJSON(ctx context.Context, method, endpoint string, payload any, output any) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return browserSessionToolError{category: mcpproto.ErrorToolError, message: "interactive session request could not be encoded"}
		}
		body = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return browserSessionToolError{category: mcpproto.ErrorToolError, message: "interactive session request could not be prepared"}
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Accept", "application/json")

	response, err := c.client.Do(request)
	if err != nil {
		return browserSessionToolError{category: mcpproto.ErrorToolError, message: "interactive browser backend request failed"}
	}
	defer response.Body.Close()

	rawBody, err := io.ReadAll(io.LimitReader(response.Body, maxChromeControlResponseBody))
	if err != nil {
		return browserSessionToolError{category: mcpproto.ErrorToolError, message: "interactive browser backend response could not be read"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return classifyChromeControlHTTPError(response.StatusCode, rawBody)
	}
	if output == nil || len(rawBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(rawBody, output); err != nil {
		return browserSessionToolError{category: mcpproto.ErrorToolError, message: "interactive browser backend response was not valid JSON"}
	}
	return nil
}

func classifyChromeControlHTTPError(statusCode int, rawBody []byte) error {
	extractMessage := func(defaultMessage string) string {
		body := map[string]any{}
		if err := json.Unmarshal(rawBody, &body); err != nil {
			return defaultMessage
		}
		if message := strings.TrimSpace(optionalString(body, "message")); message != "" {
			return message
		}
		if message := strings.TrimSpace(optionalString(body, "error")); message != "" {
			return message
		}
		return defaultMessage
	}

	switch statusCode {
	case http.StatusBadRequest:
		return browserSessionToolError{category: mcpproto.ErrorInvalidArguments, message: extractMessage("interactive session request was rejected")}
	case http.StatusNotFound:
		return browserSessionToolError{category: mcpproto.ErrorInvalidArguments, message: "interactive session token was not found"}
	case http.StatusUnauthorized, http.StatusForbidden:
		return browserSessionToolError{category: mcpproto.ErrorPermissionDenied, message: "interactive browser backend denied the request"}
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return browserSessionToolError{category: mcpproto.ErrorTimeout, message: "interactive browser backend request timed out"}
	default:
		if statusCode >= 500 {
			return browserSessionToolError{category: mcpproto.ErrorToolError, message: "interactive browser backend failed"}
		}
		return browserSessionToolError{category: mcpproto.ErrorToolError, message: extractMessage("interactive browser backend request failed")}
	}
}
