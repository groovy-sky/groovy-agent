package moderator

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ParsePlan decodes exactly one JSON Plan object from text. It rejects
// free-form prose, multiple JSON values, unknown fields, and any decision
// value outside the supported set. Callers must still call Validate before
// executing any proposed tool requests.
func ParsePlan(text string) (Plan, error) {
	candidate, ok := extractJSONObject(text)
	if !ok {
		return Plan{}, errors.New("moderator response is not a single JSON object")
	}
	decoder := json.NewDecoder(strings.NewReader(candidate))
	decoder.DisallowUnknownFields()
	var plan Plan
	if err := decoder.Decode(&plan); err != nil {
		return Plan{}, fmt.Errorf("decode plan: %w", err)
	}
	if decoder.More() {
		return Plan{}, errors.New("trailing content after plan JSON object")
	}
	switch plan.Decision {
	case DecisionAnswer, DecisionUseTools, DecisionClarify, DecisionReject:
	default:
		return Plan{}, fmt.Errorf("unsupported decision %q", plan.Decision)
	}
	return plan, nil
}

// extractJSONObject returns the trimmed text when it is exactly one JSON
// object (optionally fenced as ```json ... ```), and false otherwise. This
// mirrors the standalone JSON tool-call envelope already accepted elsewhere
// in the agent so a moderator plan may use the same fenced-or-bare style.
func extractJSONObject(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "", false
	}
	if strings.HasPrefix(trimmed, "```") {
		fenceEnd := strings.LastIndex(trimmed, "```")
		if fenceEnd <= 2 {
			return "", false
		}
		rest := strings.TrimSpace(trimmed[fenceEnd+3:])
		if rest != "" {
			return "", false
		}
		newline := strings.IndexByte(trimmed, '\n')
		if newline == -1 {
			return "", false
		}
		language := strings.TrimSpace(trimmed[3:newline])
		if language != "" && !strings.EqualFold(language, "json") {
			return "", false
		}
		trimmed = strings.TrimSpace(trimmed[newline+1 : fenceEnd])
	}
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		return "", false
	}
	var payload map[string]any
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	if err := decoder.Decode(&payload); err != nil {
		return "", false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return "", false
	}
	return trimmed, true
}
