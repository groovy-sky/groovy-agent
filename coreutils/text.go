package coreutils

import (
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Counts holds the result of WordCount.
type Counts struct {
	Lines int `json:"lines"`
	Words int `json:"words"`
	Bytes int `json:"bytes"`
}

// Head returns the first n lines of text.
func Head(text string, n int) (string, bool) {
	lines := SplitLines(text)
	truncated := false
	if n >= 0 && len(lines) > n {
		lines = lines[:n]
		truncated = true
	}
	clamped, cut := ClampLines(lines)
	return JoinLines(clamped), truncated || cut
}

// Tail returns the last n lines of text.
func Tail(text string, n int) (string, bool) {
	lines := SplitLines(text)
	truncated := false
	if n >= 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
		truncated = true
	}
	clamped, cut := ClampLines(lines)
	return JoinLines(clamped), truncated || cut
}

// WordCount counts lines, whitespace separated words, and bytes.
func WordCount(text string) Counts {
	counts := Counts{Bytes: len(text)}
	counts.Words = len(strings.Fields(text))
	counts.Lines = strings.Count(text, "\n")
	if text != "" && !strings.HasSuffix(text, "\n") {
		counts.Lines++
	}
	return counts
}

// Sort sorts the lines of text.
func Sort(text string, reverse, numeric, unique bool) string {
	lines := SplitLines(text)
	sorted := make([]string, len(lines))
	copy(sorted, lines)
	less := func(i, j int) bool { return sorted[i] < sorted[j] }
	if numeric {
		less = func(i, j int) bool {
			left, leftErr := strconv.ParseFloat(strings.TrimSpace(sorted[i]), 64)
			right, rightErr := strconv.ParseFloat(strings.TrimSpace(sorted[j]), 64)
			if leftErr != nil || rightErr != nil {
				return sorted[i] < sorted[j]
			}
			return left < right
		}
	}
	sort.SliceStable(sorted, less)
	if reverse {
		for i, j := 0, len(sorted)-1; i < j; i, j = i+1, j-1 {
			sorted[i], sorted[j] = sorted[j], sorted[i]
		}
	}
	if unique {
		sorted = uniqueAdjacent(sorted)
	}
	return JoinLines(sorted)
}

func uniqueAdjacent(lines []string) []string {
	out := make([]string, 0, len(lines))
	for index, line := range lines {
		if index > 0 && line == lines[index-1] {
			continue
		}
		out = append(out, line)
	}
	return out
}

// Uniq removes adjacent duplicate lines, optionally prefixing counts.
func Uniq(text string, count bool) string {
	lines := SplitLines(text)
	out := make([]string, 0, len(lines))
	index := 0
	for index < len(lines) {
		run := 1
		for index+run < len(lines) && lines[index+run] == lines[index] {
			run++
		}
		if count {
			out = append(out, fmt.Sprintf("%7d %s", run, lines[index]))
		} else {
			out = append(out, lines[index])
		}
		index += run
	}
	return JoinLines(out)
}

// Cut selects delimiter separated fields from each line. Fields are 1-based.
func Cut(text, delimiter string, fields []int) (string, error) {
	if delimiter == "" {
		return "", errors.New("delimiter must not be empty")
	}
	if len(fields) == 0 {
		return "", errors.New("at least one field is required")
	}
	for _, field := range fields {
		if field < 1 {
			return "", errors.New("fields must be 1-based positive integers")
		}
	}
	lines := SplitLines(text)
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		parts := strings.Split(line, delimiter)
		selected := make([]string, 0, len(fields))
		for _, field := range fields {
			if field <= len(parts) {
				selected = append(selected, parts[field-1])
			}
		}
		out = append(out, strings.Join(selected, delimiter))
	}
	return JoinLines(out), nil
}

// Paste merges the inputs line by line using the delimiter.
func Paste(inputs []string, delimiter string) (string, error) {
	if len(inputs) < 1 {
		return "", errors.New("at least one input is required")
	}
	if delimiter == "" {
		delimiter = "\t"
	}
	columns := make([][]string, len(inputs))
	longest := 0
	for index, input := range inputs {
		columns[index] = SplitLines(input)
		if len(columns[index]) > longest {
			longest = len(columns[index])
		}
	}
	out := make([]string, 0, longest)
	for row := 0; row < longest; row++ {
		cells := make([]string, len(columns))
		for column := range columns {
			if row < len(columns[column]) {
				cells[column] = columns[column][row]
			}
		}
		out = append(out, strings.Join(cells, delimiter))
	}
	return JoinLines(out), nil
}

// Tr translates or deletes characters.
func Tr(text, from, to string, delete bool) (string, error) {
	source := []rune(from)
	if len(source) == 0 {
		return "", errors.New("source character set must not be empty")
	}
	if delete {
		set := make(map[rune]struct{}, len(source))
		for _, character := range source {
			set[character] = struct{}{}
		}
		var builder strings.Builder
		for _, character := range text {
			if _, drop := set[character]; drop {
				continue
			}
			builder.WriteRune(character)
		}
		return builder.String(), nil
	}
	target := []rune(to)
	if len(target) == 0 {
		return "", errors.New("target character set must not be empty")
	}
	mapping := make(map[rune]rune, len(source))
	for index, character := range source {
		if index < len(target) {
			mapping[character] = target[index]
		} else {
			mapping[character] = target[len(target)-1]
		}
	}
	var builder strings.Builder
	for _, character := range text {
		if replacement, ok := mapping[character]; ok {
			builder.WriteRune(replacement)
			continue
		}
		builder.WriteRune(character)
	}
	return builder.String(), nil
}

// Base64Encode encodes data using standard base64.
func Base64Encode(data string) string {
	return base64.StdEncoding.EncodeToString([]byte(data))
}

// Base64Decode decodes standard base64 text and rejects non-textual results so
// raw binary data never reaches the model.
func Base64Decode(data string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(data))
	if err != nil {
		return "", errors.New("input is not valid base64")
	}
	if !utf8.Valid(decoded) {
		return "", errors.New("decoded data is not valid UTF-8 text")
	}
	return string(decoded), nil
}
