package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ApplyPatchResult is returned from ApplyPatch.
type ApplyPatchResult struct {
	Files []string `json:"files"`
}

type patchFile struct {
	Path  string
	Hunks []patchHunk
}

type patchHunk struct {
	OldStart int
	Lines    []patchLine
}

type patchLine struct {
	Kind byte
	Text string
}

// ApplyPatch applies a bounded subset of unified diffs to regular files inside
// the workspace. Only same-path edits are supported; file creates/deletes and
// renames are rejected.
func (ws *Workspace) ApplyPatch(patchText string) (ApplyPatchResult, error) {
	files, err := parseUnifiedPatch(patchText)
	if err != nil {
		return ApplyPatchResult{}, err
	}
	updated := make(map[string]string, len(files))
	for _, filePatch := range files {
		resolved, err := ws.ResolveExistingPath(filePatch.Path)
		if err != nil {
			return ApplyPatchResult{}, err
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return ApplyPatchResult{}, err
		}
		if !info.Mode().IsRegular() {
			return ApplyPatchResult{}, fmt.Errorf("patch target %q is not a regular file", filePatch.Path)
		}
		contents, err := os.ReadFile(resolved)
		if err != nil {
			return ApplyPatchResult{}, err
		}
		applied, err := applyPatchToContent(string(contents), filePatch)
		if err != nil {
			return ApplyPatchResult{}, fmt.Errorf("apply patch to %s: %w", filePatch.Path, err)
		}
		updated[filePatch.Path] = applied
	}
	for path, contents := range updated {
		if err := ws.WriteFile(path, contents); err != nil {
			return ApplyPatchResult{}, err
		}
	}
	paths := make([]string, 0, len(updated))
	for path := range updated {
		paths = append(paths, path)
	}
	return ApplyPatchResult{Files: paths}, nil
}

func parseUnifiedPatch(patchText string) ([]patchFile, error) {
	if strings.Contains(patchText, "GIT binary patch") || strings.Contains(patchText, "Binary files") {
		return nil, errors.New("binary patches are not supported")
	}
	lines := strings.Split(strings.ReplaceAll(patchText, "\r\n", "\n"), "\n")
	files := make([]patchFile, 0)
	var current *patchFile
	for index := 0; index < len(lines); index++ {
		line := lines[index]
		switch {
		case strings.HasPrefix(line, "rename "), strings.HasPrefix(line, "copy "), strings.HasPrefix(line, "new file mode"), strings.HasPrefix(line, "deleted file mode"):
			return nil, errors.New("rename/copy/new/delete metadata is not supported")
		case strings.HasPrefix(line, "diff --git "):
			continue
		case strings.HasPrefix(line, "--- "):
			if index+1 >= len(lines) || !strings.HasPrefix(lines[index+1], "+++ ") {
				return nil, errors.New("malformed patch: missing +++ header")
			}
			oldPath := strings.TrimSpace(strings.TrimPrefix(line, "--- "))
			newPath := strings.TrimSpace(strings.TrimPrefix(lines[index+1], "+++ "))
			if oldPath == "/dev/null" || newPath == "/dev/null" {
				return nil, errors.New("file create/delete patches are not supported; use write_file")
			}
			oldPath = normalizePatchPath(oldPath)
			newPath = normalizePatchPath(newPath)
			if oldPath != newPath {
				return nil, fmt.Errorf("path rename in patch is not supported: %s -> %s", oldPath, newPath)
			}
			files = append(files, patchFile{Path: oldPath})
			current = &files[len(files)-1]
			index++
		case strings.HasPrefix(line, "@@ "):
			if current == nil {
				return nil, errors.New("malformed patch: hunk without file header")
			}
			oldStart, err := parseOldStart(line)
			if err != nil {
				return nil, err
			}
			hunk := patchHunk{OldStart: oldStart}
			for index+1 < len(lines) {
				next := lines[index+1]
				if strings.HasPrefix(next, "@@ ") || strings.HasPrefix(next, "--- ") || strings.HasPrefix(next, "diff --git ") {
					break
				}
				index++
				if next == "" && index == len(lines)-1 {
					break
				}
				if strings.HasPrefix(next, "\\ No newline at end of file") {
					return nil, errors.New("patch marker '\\ No newline at end of file' is not supported")
				}
				if len(next) == 0 {
					hunk.Lines = append(hunk.Lines, patchLine{Kind: ' ', Text: ""})
					continue
				}
				kind := next[0]
				if kind != ' ' && kind != '+' && kind != '-' {
					return nil, fmt.Errorf("malformed hunk line: %q", next)
				}
				hunk.Lines = append(hunk.Lines, patchLine{Kind: kind, Text: next[1:]})
			}
			current.Hunks = append(current.Hunks, hunk)
		}
	}
	if len(files) == 0 {
		return nil, errors.New("no file changes found in patch")
	}
	for _, filePatch := range files {
		if len(filePatch.Hunks) == 0 {
			return nil, fmt.Errorf("patch for %s has no hunks", filePatch.Path)
		}
	}
	return files, nil
}

func parseOldStart(header string) (int, error) {
	parts := strings.Split(header, " ")
	if len(parts) < 3 {
		return 0, fmt.Errorf("malformed hunk header %q", header)
	}
	oldRange := strings.TrimPrefix(parts[1], "-")
	comma := strings.IndexByte(oldRange, ',')
	if comma >= 0 {
		oldRange = oldRange[:comma]
	}
	value, err := strconv.Atoi(oldRange)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("malformed hunk header %q", header)
	}
	return value, nil
}

func normalizePatchPath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.TrimPrefix(path, "a/")
	path = strings.TrimPrefix(path, "b/")
	return filepath.ToSlash(path)
}

func applyPatchToContent(content string, filePatch patchFile) (string, error) {
	hasTrailingNewline := strings.HasSuffix(content, "\n")
	base := strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(base, "\n")
	if hasTrailingNewline && len(lines) > 0 {
		lines = lines[:len(lines)-1]
	}
	result := make([]string, 0, len(lines))
	position := 0
	for _, hunk := range filePatch.Hunks {
		target := hunk.OldStart - 1
		if target < position || target > len(lines) {
			return "", fmt.Errorf("hunk start %d out of range", hunk.OldStart)
		}
		result = append(result, lines[position:target]...)
		cursor := target
		for _, line := range hunk.Lines {
			switch line.Kind {
			case ' ':
				if cursor >= len(lines) || lines[cursor] != line.Text {
					return "", fmt.Errorf("context mismatch at line %d", cursor+1)
				}
				result = append(result, lines[cursor])
				cursor++
			case '-':
				if cursor >= len(lines) || lines[cursor] != line.Text {
					return "", fmt.Errorf("delete mismatch at line %d", cursor+1)
				}
				cursor++
			case '+':
				result = append(result, line.Text)
			default:
				return "", fmt.Errorf("unexpected hunk line kind %q", string(line.Kind))
			}
		}
		position = cursor
	}
	result = append(result, lines[position:]...)
	joined := strings.Join(result, "\n")
	if hasTrailingNewline {
		joined += "\n"
	}
	return joined, nil
}
