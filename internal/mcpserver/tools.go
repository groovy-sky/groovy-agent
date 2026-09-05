package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/groovy-sky/groovy-agent/coreutils"
	"github.com/groovy-sky/groovy-agent/internal/jsonschema"
	"github.com/groovy-sky/groovy-agent/internal/mcpproto"
)

// maxHashBytes bounds sha256sum so hashing always fits the execution budget.
const maxHashBytes = 1 << 20

// WriteCapableTools are intentionally not implemented by this server. They are
// listed so the policy is explicit and testable.
var WriteCapableTools = []string{"cp", "link", "mkdir", "rmdir", "tee", "touch", "unlink"}

func object(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		names := make([]any, 0, len(required))
		for _, name := range required {
			names = append(names, name)
		}
		schema["required"] = names
	}
	return schema
}

func stringField(description string, maxLength int) map[string]any {
	return map[string]any{"type": "string", "description": description, "maxLength": maxLength}
}

func boolField(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

func intField(description string, minimum, maximum int) map[string]any {
	return map[string]any{"type": "integer", "description": description, "minimum": minimum, "maximum": maximum}
}

func pathField() map[string]any {
	return stringField("Workspace-relative file path.", 512)
}

func textField() map[string]any {
	return stringField("Input text (bounded).", 12<<10)
}

func definitions() []tool {
	return []tool{
		{
			name:        "pwd",
			description: "Print the logical workspace path.",
			schema:      object(map[string]any{}),
			run:         runPwd,
		},
		{
			name:        "date",
			description: "Print the current date and time.",
			schema: object(map[string]any{
				"utc": boolField("Use UTC instead of local time."),
			}),
			run: runDate,
		},
		{
			name:        "cat",
			description: "Read a bounded amount of a workspace text file.",
			schema: object(map[string]any{
				"path":      pathField(),
				"max_bytes": intField("Maximum bytes to read.", 1, 12<<10),
			}, "path"),
			run: runCat,
		},
		{
			name:        "head",
			description: "Read the first lines of a workspace file.",
			schema: object(map[string]any{
				"path":  pathField(),
				"lines": intField("Number of leading lines.", 1, 200),
			}, "path"),
			run: runHead,
		},
		{
			name:        "tail",
			description: "Read the last lines of a workspace file.",
			schema: object(map[string]any{
				"path":  pathField(),
				"lines": intField("Number of trailing lines.", 1, 200),
			}, "path"),
			run: runTail,
		},
		{
			name:        "wc",
			description: "Count lines, words, and bytes of a file or text.",
			schema: object(map[string]any{
				"path": pathField(),
				"text": textField(),
			}),
			run: runWC,
		},
		{
			name:        "grep",
			description: "Search a workspace file for a pattern; matches are limited.",
			schema: object(map[string]any{
				"path": pathField(),
				"pattern": map[string]any{
					"type":        "string",
					"description": "Pattern to search for.",
					"minLength":   1,
					"maxLength":   256,
				},
				"ignore_case": boolField("Case-insensitive search."),
				"fixed":       boolField("Treat the pattern as literal text."),
				"max_matches": intField("Maximum number of matches.", 1, 20),
			}, "path", "pattern"),
			run: runGrep,
		},
		{
			name:        "sha256sum",
			description: "Compute the SHA-256 digest of a workspace file.",
			schema:      object(map[string]any{"path": pathField()}, "path"),
			run:         runSha256Sum,
		},
		{
			name:        "basename",
			description: "Strip the directory and an optional suffix from a path.",
			schema: object(map[string]any{
				"path":   stringField("Path to reduce.", 512),
				"suffix": stringField("Optional suffix to remove.", 64),
			}, "path"),
			run: runBasename,
		},
		{
			name:        "dirname",
			description: "Strip the last component from a path.",
			schema:      object(map[string]any{"path": stringField("Path to reduce.", 512)}, "path"),
			run:         runDirname,
		},
		{
			name:        "base64",
			description: "Encode or decode base64 text.",
			schema: object(map[string]any{
				"text":   textField(),
				"decode": boolField("Decode instead of encode."),
			}, "text"),
			run: runBase64,
		},
		{
			name:        "cut",
			description: "Select delimiter separated fields from each line.",
			schema: object(map[string]any{
				"text":      textField(),
				"delimiter": stringField("Field delimiter.", 8),
				"fields": map[string]any{
					"type":        "array",
					"description": "1-based field numbers.",
					"items":       map[string]any{"type": "integer", "minimum": 1, "maximum": 1024},
					"minItems":    1,
					"maxItems":    16,
				},
			}, "text", "delimiter", "fields"),
			run: runCut,
		},
		{
			name:        "paste",
			description: "Merge several texts line by line.",
			schema: object(map[string]any{
				"inputs": map[string]any{
					"type":        "array",
					"description": "Texts to merge.",
					"items":       textField(),
					"minItems":    1,
					"maxItems":    4,
				},
				"delimiter": stringField("Column delimiter (default tab).", 8),
			}, "inputs"),
			run: runPaste,
		},
		{
			name:        "sort",
			description: "Sort the lines of the supplied text.",
			schema: object(map[string]any{
				"text":    textField(),
				"reverse": boolField("Sort in descending order."),
				"numeric": boolField("Compare lines numerically."),
				"unique":  boolField("Drop duplicate lines."),
			}, "text"),
			run: runSort,
		},
		{
			name:        "tr",
			description: "Translate or delete characters in the supplied text.",
			schema: object(map[string]any{
				"text":   textField(),
				"from":   stringField("Characters to translate or delete.", 256),
				"to":     stringField("Replacement characters.", 256),
				"delete": boolField("Delete the characters instead of translating."),
			}, "text", "from"),
			run: runTr,
		},
		{
			name:        "uniq",
			description: "Remove adjacent duplicate lines.",
			schema: object(map[string]any{
				"text":  textField(),
				"count": boolField("Prefix each line with its repetition count."),
			}, "text"),
			run: runUniq,
		},
	}
}

func optionalBool(arguments map[string]any, key string) bool {
	value, _ := arguments[key].(bool)
	return value
}

func optionalInt(arguments map[string]any, key string, fallback int) int {
	value, ok := arguments[key]
	if !ok {
		return fallback
	}
	number, ok := jsonschema.Number(value)
	if !ok {
		return fallback
	}
	return number
}

func requireString(arguments map[string]any, key string) (string, error) {
	value, ok := arguments[key].(string)
	if !ok {
		return "", fail(mcpproto.ErrorInvalidArguments, "%q must be a string", key)
	}
	return value, nil
}

func runPwd(_ context.Context, s *Server, _ map[string]any) (payload, error) {
	logical := "/" + filepath.Base(s.workspace)
	return payload{
		Output:   logical,
		Metadata: map[string]any{"workspace": logical, "relative_root": "."},
	}, nil
}

func runDate(_ context.Context, _ *Server, arguments map[string]any) (payload, error) {
	now := time.Now()
	if optionalBool(arguments, "utc") {
		now = now.UTC()
	}
	return payload{Output: now.Format(time.RFC3339), Metadata: map[string]any{"format": "RFC3339"}}, nil
}

func runCat(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	path, err := requireString(arguments, "path")
	if err != nil {
		return payload{}, err
	}
	limit := optionalInt(arguments, "max_bytes", s.limits.MaxFileReadBytes)
	content, truncated, err := s.readFile(path, limit)
	if err != nil {
		return payload{}, err
	}
	lines, clamped := coreutils.ClampLines(coreutils.SplitLines(content))
	output, cut := coreutils.Clamp(coreutils.JoinLines(lines), limit)
	return payload{
		Output:    output,
		Truncated: truncated || clamped || cut,
		Metadata:  map[string]any{"path": path, "bytes": len(content)},
	}, nil
}

func runHead(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	path, err := requireString(arguments, "path")
	if err != nil {
		return payload{}, err
	}
	count := optionalInt(arguments, "lines", 20)
	content, truncated, err := s.readFile(path, s.limits.MaxFileReadBytes)
	if err != nil {
		return payload{}, err
	}
	output, cut := coreutils.Head(content, count)
	return payload{
		Output:    output,
		Truncated: truncated || cut,
		Metadata:  map[string]any{"path": path, "lines": count},
	}, nil
}

func runTail(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	path, err := requireString(arguments, "path")
	if err != nil {
		return payload{}, err
	}
	count := optionalInt(arguments, "lines", 20)
	content, truncated, err := s.readFile(path, s.limits.MaxFileReadBytes)
	if err != nil {
		return payload{}, err
	}
	output, cut := coreutils.Tail(content, count)
	return payload{
		Output:    output,
		Truncated: truncated || cut,
		Metadata:  map[string]any{"path": path, "lines": count},
	}, nil
}

func runWC(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	_, hasPath := arguments["path"]
	_, hasText := arguments["text"]
	if hasPath == hasText {
		return payload{}, fail(mcpproto.ErrorInvalidArguments, "provide exactly one of \"path\" or \"text\"")
	}
	content := ""
	truncated := false
	if hasPath {
		path, err := requireString(arguments, "path")
		if err != nil {
			return payload{}, err
		}
		content, truncated, err = s.readFile(path, s.limits.MaxFileReadBytes)
		if err != nil {
			return payload{}, err
		}
	} else {
		text, err := s.requireText(arguments, "text")
		if err != nil {
			return payload{}, err
		}
		content = text
	}
	counts := coreutils.WordCount(content)
	return payload{
		Output:    fmt.Sprintf("%d lines %d words %d bytes", counts.Lines, counts.Words, counts.Bytes),
		Truncated: truncated,
		Metadata:  map[string]any{"lines": counts.Lines, "words": counts.Words, "bytes": counts.Bytes},
	}, nil
}

func runGrep(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	path, err := requireString(arguments, "path")
	if err != nil {
		return payload{}, err
	}
	pattern, err := requireString(arguments, "pattern")
	if err != nil {
		return payload{}, err
	}
	maxMatches := optionalInt(arguments, "max_matches", s.limits.MaxGrepMatches)
	if maxMatches > s.limits.MaxGrepMatches {
		maxMatches = s.limits.MaxGrepMatches
	}
	content, truncated, err := s.readFile(path, s.limits.MaxFileReadBytes)
	if err != nil {
		return payload{}, err
	}
	matches, cut, err := coreutils.Grep(content, coreutils.GrepOptions{
		Pattern:    pattern,
		IgnoreCase: optionalBool(arguments, "ignore_case"),
		FixedText:  optionalBool(arguments, "fixed"),
		MaxMatches: maxMatches,
	})
	if err != nil {
		return payload{}, fail(mcpproto.ErrorInvalidArguments, "%s", err.Error())
	}
	lines := make([]string, 0, len(matches))
	for _, match := range matches {
		lines = append(lines, fmt.Sprintf("%d:%s", match.Line, match.Text))
	}
	return payload{
		Output:    coreutils.JoinLines(lines),
		Truncated: truncated || cut,
		Metadata:  map[string]any{"path": path, "matches": len(matches)},
	}, nil
}

func runSha256Sum(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	relative, err := requireString(arguments, "path")
	if err != nil {
		return payload{}, err
	}
	path, err := s.resolvePath(relative)
	if err != nil {
		return payload{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return payload{}, fail(mcpproto.ErrorToolError, "file could not be inspected")
	}
	if !info.Mode().IsRegular() {
		return payload{}, fail(mcpproto.ErrorInvalidArguments, "path is not a regular file")
	}
	if info.Size() > maxHashBytes {
		return payload{}, fail(mcpproto.ErrorResultTooLarge, "file is too large to hash within the execution budget")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return payload{}, fail(mcpproto.ErrorPermissionDenied, "file is not readable")
		}
		return payload{}, fail(mcpproto.ErrorToolError, "file could not be read")
	}
	return payload{
		Output:   coreutils.Sha256Sum(data),
		Metadata: map[string]any{"path": relative, "bytes": len(data)},
	}, nil
}

func runBasename(_ context.Context, _ *Server, arguments map[string]any) (payload, error) {
	path, err := requireString(arguments, "path")
	if err != nil {
		return payload{}, err
	}
	suffix, _ := arguments["suffix"].(string)
	return payload{Output: coreutils.Basename(path, suffix)}, nil
}

func runDirname(_ context.Context, _ *Server, arguments map[string]any) (payload, error) {
	path, err := requireString(arguments, "path")
	if err != nil {
		return payload{}, err
	}
	return payload{Output: coreutils.Dirname(path)}, nil
}

func runBase64(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	text, err := s.requireText(arguments, "text")
	if err != nil {
		return payload{}, err
	}
	if optionalBool(arguments, "decode") {
		decoded, decodeErr := coreutils.Base64Decode(text)
		if decodeErr != nil {
			return payload{}, fail(mcpproto.ErrorInvalidArguments, "%s", decodeErr.Error())
		}
		return payload{Output: decoded}, nil
	}
	return payload{Output: coreutils.Base64Encode(text)}, nil
}

func runCut(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	text, err := s.requireText(arguments, "text")
	if err != nil {
		return payload{}, err
	}
	delimiter, err := requireString(arguments, "delimiter")
	if err != nil {
		return payload{}, err
	}
	rawFields, ok := arguments["fields"].([]any)
	if !ok {
		return payload{}, fail(mcpproto.ErrorInvalidArguments, "\"fields\" must be an array of integers")
	}
	fields := make([]int, 0, len(rawFields))
	for _, raw := range rawFields {
		field, ok := jsonschema.Number(raw)
		if !ok {
			return payload{}, fail(mcpproto.ErrorInvalidArguments, "\"fields\" must contain integers")
		}
		fields = append(fields, field)
	}
	output, err := coreutils.Cut(text, delimiter, fields)
	if err != nil {
		return payload{}, fail(mcpproto.ErrorInvalidArguments, "%s", err.Error())
	}
	return payload{Output: output}, nil
}

func runPaste(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	rawInputs, ok := arguments["inputs"].([]any)
	if !ok {
		return payload{}, fail(mcpproto.ErrorInvalidArguments, "\"inputs\" must be an array of strings")
	}
	total := 0
	inputs := make([]string, 0, len(rawInputs))
	for _, raw := range rawInputs {
		text, ok := raw.(string)
		if !ok {
			return payload{}, fail(mcpproto.ErrorInvalidArguments, "\"inputs\" must contain strings")
		}
		total += len(text)
		if total > s.limits.MaxFileReadBytes {
			return payload{}, fail(mcpproto.ErrorInvalidArguments, "input text exceeds the allowed size")
		}
		inputs = append(inputs, text)
	}
	delimiter, _ := arguments["delimiter"].(string)
	output, err := coreutils.Paste(inputs, delimiter)
	if err != nil {
		return payload{}, fail(mcpproto.ErrorInvalidArguments, "%s", err.Error())
	}
	return payload{Output: output}, nil
}

func runSort(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	text, err := s.requireText(arguments, "text")
	if err != nil {
		return payload{}, err
	}
	output := coreutils.Sort(text,
		optionalBool(arguments, "reverse"),
		optionalBool(arguments, "numeric"),
		optionalBool(arguments, "unique"),
	)
	return payload{Output: output}, nil
}

func runTr(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	text, err := s.requireText(arguments, "text")
	if err != nil {
		return payload{}, err
	}
	from, err := requireString(arguments, "from")
	if err != nil {
		return payload{}, err
	}
	to, _ := arguments["to"].(string)
	output, err := coreutils.Tr(text, from, to, optionalBool(arguments, "delete"))
	if err != nil {
		return payload{}, fail(mcpproto.ErrorInvalidArguments, "%s", err.Error())
	}
	return payload{Output: output}, nil
}

func runUniq(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	text, err := s.requireText(arguments, "text")
	if err != nil {
		return payload{}, err
	}
	return payload{Output: coreutils.Uniq(text, optionalBool(arguments, "count"))}, nil
}
