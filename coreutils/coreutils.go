// Package coreutils provides small, bounded implementations of common Unix
// utilities. Every helper works on in-memory data that the caller has already
// limited, and every helper reports whether its result was truncated.
//
// The helpers never execute a shell and never touch the filesystem; path and
// file access is the responsibility of the MCP server that uses them.
package coreutils

import "strings"

// MaxLineLength is the maximum number of bytes kept for a single line.
const MaxLineLength = 2 << 10 // 2 KiB

// SplitLines splits text into lines, dropping a single trailing newline.
func SplitLines(text string) []string {
	if text == "" {
		return nil
	}
	text = strings.TrimSuffix(text, "\n")
	return strings.Split(text, "\n")
}

// JoinLines joins lines with newlines, appending a trailing newline when the
// result is not empty.
func JoinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// ClampLine truncates a single line to MaxLineLength bytes.
func ClampLine(line string) (string, bool) {
	if len(line) <= MaxLineLength {
		return line, false
	}
	return line[:MaxLineLength], true
}

// ClampLines truncates every line and reports whether any line was truncated.
func ClampLines(lines []string) ([]string, bool) {
	truncated := false
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		clamped, cut := ClampLine(line)
		truncated = truncated || cut
		out = append(out, clamped)
	}
	return out, truncated
}

// Clamp truncates text to at most limit bytes and reports whether bytes were
// dropped.
func Clamp(text string, limit int) (string, bool) {
	if limit < 0 {
		limit = 0
	}
	if len(text) <= limit {
		return text, false
	}
	return text[:limit], true
}
