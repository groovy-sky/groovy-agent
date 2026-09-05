package coreutils

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Match is a single grep match.
type Match struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

// GrepOptions bounds a grep invocation.
type GrepOptions struct {
	Pattern    string
	IgnoreCase bool
	FixedText  bool
	MaxMatches int
}

// Grep searches text line by line and stops after MaxMatches matches.
func Grep(text string, options GrepOptions) ([]Match, bool, error) {
	if options.Pattern == "" {
		return nil, false, errors.New("pattern must not be empty")
	}
	if len(options.Pattern) > 256 {
		return nil, false, errors.New("pattern is too long")
	}
	if options.MaxMatches <= 0 {
		options.MaxMatches = 20
	}

	var matcher func(string) bool
	if options.FixedText {
		needle := options.Pattern
		if options.IgnoreCase {
			needle = strings.ToLower(needle)
			matcher = func(line string) bool { return strings.Contains(strings.ToLower(line), needle) }
		} else {
			matcher = func(line string) bool { return strings.Contains(line, needle) }
		}
	} else {
		pattern := options.Pattern
		if options.IgnoreCase {
			pattern = "(?i)" + pattern
		}
		expression, err := regexp.Compile(pattern)
		if err != nil {
			return nil, false, fmt.Errorf("invalid pattern: %s", err)
		}
		expression.Longest()
		matcher = expression.MatchString
	}

	matches := make([]Match, 0, options.MaxMatches)
	truncated := false
	for index, line := range SplitLines(text) {
		if !matcher(line) {
			continue
		}
		if len(matches) >= options.MaxMatches {
			truncated = true
			break
		}
		clamped, cut := ClampLine(line)
		truncated = truncated || cut
		matches = append(matches, Match{Line: index + 1, Text: clamped})
	}
	return matches, truncated, nil
}
