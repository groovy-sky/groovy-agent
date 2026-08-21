package workspace

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	DefaultMaxFileSizeBytes  int64 = 1 << 20
	DefaultMaxOutputBytes          = 256 << 10
	DefaultMaxTraversalDepth       = 6
	DefaultMaxSearchResults        = 200
	DefaultMaxListEntries          = 2000
)

type Limits struct {
	MaxFileSizeBytes  int64
	MaxOutputBytes    int
	MaxTraversalDepth int
	MaxSearchResults  int
	MaxListEntries    int
}

func DefaultLimits() Limits {
	return Limits{
		MaxFileSizeBytes:  DefaultMaxFileSizeBytes,
		MaxOutputBytes:    DefaultMaxOutputBytes,
		MaxTraversalDepth: DefaultMaxTraversalDepth,
		MaxSearchResults:  DefaultMaxSearchResults,
		MaxListEntries:    DefaultMaxListEntries,
	}
}

type Workspace struct {
	Root   string
	Limits Limits
}

type FileInfo struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

func New(root string, limits Limits) (*Workspace, error) {
	if strings.TrimSpace(root) == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("get working directory: %w", err)
		}
		root = cwd
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace path: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace symlinks: %w", err)
	}
	info, err := os.Stat(resolvedRoot)
	if err != nil {
		return nil, fmt.Errorf("access workspace root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace root is not a directory: %s", resolvedRoot)
	}
	limits = normalizeLimits(limits)
	return &Workspace{Root: filepath.Clean(resolvedRoot), Limits: limits}, nil
}

func normalizeLimits(l Limits) Limits {
	defaults := DefaultLimits()
	if l.MaxFileSizeBytes <= 0 {
		l.MaxFileSizeBytes = defaults.MaxFileSizeBytes
	}
	if l.MaxOutputBytes <= 0 {
		l.MaxOutputBytes = defaults.MaxOutputBytes
	}
	if l.MaxTraversalDepth <= 0 {
		l.MaxTraversalDepth = defaults.MaxTraversalDepth
	}
	if l.MaxSearchResults <= 0 {
		l.MaxSearchResults = defaults.MaxSearchResults
	}
	if l.MaxListEntries <= 0 {
		l.MaxListEntries = defaults.MaxListEntries
	}
	return l
}

func (workspace *Workspace) ResolvePath(path string) (string, error) {
	return workspace.resolve(path, true)
}

func (workspace *Workspace) ResolveExistingPath(path string) (string, error) {
	return workspace.resolve(path, false)
}

func (workspace *Workspace) NormalizeRelativePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q must be workspace-relative", path)
	}
	resolved, err := workspace.ResolvePath(path)
	if err != nil {
		return "", err
	}
	rel := relativeFromRoot(workspace.Root, resolved)
	if rel == "." {
		return "", fmt.Errorf("path %q must point to a file inside the workspace", path)
	}
	return rel, nil
}

func (workspace *Workspace) resolve(path string, allowMissing bool) (string, error) {
	if strings.TrimSpace(path) == "" {
		path = "."
	}
	cleanInput := filepath.Clean(path)
	if hasDotDot(cleanInput) {
		return "", fmt.Errorf("path %q contains '..' and is not allowed", path)
	}

	var lexical string
	if filepath.IsAbs(cleanInput) {
		lexical = cleanInput
	} else {
		lexical = filepath.Join(workspace.Root, cleanInput)
	}
	lexical = filepath.Clean(lexical)
	if !isWithin(workspace.Root, lexical) {
		return "", fmt.Errorf("path %q is outside workspace %q", path, workspace.Root)
	}

	rel, err := filepath.Rel(workspace.Root, lexical)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	if rel == "." {
		return workspace.Root, nil
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	current := workspace.Root
	for idx, part := range parts {
		if part == "" || part == "." {
			continue
		}
		nextPath := filepath.Join(current, part)
		info, statErr := os.Lstat(nextPath)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				if !allowMissing {
					return "", fmt.Errorf("path %q does not exist", path)
				}
				for rest := idx; rest < len(parts); rest++ {
					if parts[rest] == "" || parts[rest] == "." {
						continue
					}
					current = filepath.Join(current, parts[rest])
				}
				break
			}
			return "", fmt.Errorf("access path %q: %w", path, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, resolveErr := filepath.EvalSymlinks(nextPath)
			if resolveErr != nil {
				return "", fmt.Errorf("resolve symlink at %q: %w", path, resolveErr)
			}
			resolved = filepath.Clean(resolved)
			if !isWithin(workspace.Root, resolved) {
				return "", fmt.Errorf("path %q resolves outside workspace through symlink", path)
			}
			current = resolved
		} else {
			current = nextPath
		}
		if !isWithin(workspace.Root, current) {
			return "", fmt.Errorf("path %q resolves outside workspace", path)
		}
	}
	current = filepath.Clean(current)
	if !isWithin(workspace.Root, current) {
		return "", fmt.Errorf("path %q resolves outside workspace", path)
	}
	return current, nil
}

func hasDotDot(cleanPath string) bool {
	if cleanPath == ".." {
		return true
	}
	parts := strings.Split(cleanPath, string(os.PathSeparator))
	for _, part := range parts {
		if part == ".." {
			return true
		}
	}
	return false
}

func isWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, "..")
}

type ListOptions struct {
	Path    string
	Depth   int
	Include []string
	Exclude []string
}

type ListEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size,omitempty"`
}

type ListResult struct {
	Root      string      `json:"root"`
	Entries   []ListEntry `json:"entries"`
	Skipped   []string    `json:"skipped,omitempty"`
	Truncated bool        `json:"truncated,omitempty"`
}

func (workspace *Workspace) ListFiles(options ListOptions) (ListResult, error) {
	depth := options.Depth
	if depth <= 0 {
		depth = workspace.Limits.MaxTraversalDepth
	}
	if depth > workspace.Limits.MaxTraversalDepth {
		depth = workspace.Limits.MaxTraversalDepth
	}
	rootPath, err := workspace.ResolveExistingPath(options.Path)
	if err != nil {
		return ListResult{}, err
	}
	info, err := os.Stat(rootPath)
	if err != nil {
		return ListResult{}, fmt.Errorf("stat %q: %w", options.Path, err)
	}
	if !info.IsDir() {
		return ListResult{}, fmt.Errorf("path %q is not a directory", options.Path)
	}
	result := ListResult{Root: workspace.Root}
	type node struct {
		abs   string
		rel   string
		level int
	}
	queue := []node{{abs: rootPath, rel: relativeFromRoot(workspace.Root, rootPath), level: 0}}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		entries, readErr := os.ReadDir(current.abs)
		if readErr != nil {
			result.Skipped = append(result.Skipped, fmt.Sprintf("%s: %v", current.rel, readErr))
			continue
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			absPath := filepath.Join(current.abs, entry.Name())
			relPath := relativeFromRoot(workspace.Root, absPath)
			if !matchesIncludeExclude(relPath, options.Include, options.Exclude) {
				continue
			}
			fileInfo, infoErr := entry.Info()
			if infoErr != nil {
				result.Skipped = append(result.Skipped, fmt.Sprintf("%s: %v", relPath, infoErr))
				continue
			}
			mode := fileInfo.Mode()
			switch {
			case mode.IsDir():
				result.Entries = append(result.Entries, ListEntry{Path: relPath, Type: "dir"})
				if current.level+1 <= depth {
					queue = append(queue, node{abs: absPath, rel: relPath, level: current.level + 1})
				}
			case mode.IsRegular():
				result.Entries = append(result.Entries, ListEntry{Path: relPath, Type: "file", Size: fileInfo.Size()})
			default:
				result.Skipped = append(result.Skipped, fmt.Sprintf("%s: unsupported file type", relPath))
			}
			if len(result.Entries) >= workspace.Limits.MaxListEntries {
				result.Truncated = true
				sort.Slice(result.Entries, func(i, j int) bool { return result.Entries[i].Path < result.Entries[j].Path })
				sort.Strings(result.Skipped)
				return result, nil
			}
		}
	}
	sort.Slice(result.Entries, func(i, j int) bool { return result.Entries[i].Path < result.Entries[j].Path })
	sort.Strings(result.Skipped)
	return result, nil
}

func matchesIncludeExclude(path string, includes, excludes []string) bool {
	path = filepath.ToSlash(path)
	if len(includes) > 0 {
		matched := false
		for _, pattern := range includes {
			ok, err := filepath.Match(pattern, path)
			if err == nil && ok {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for _, pattern := range excludes {
		ok, err := filepath.Match(pattern, path)
		if err == nil && ok {
			return false
		}
	}
	return true
}

func relativeFromRoot(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return "."
	}
	return filepath.ToSlash(rel)
}

type ReadResult struct {
	Path      string     `json:"path"`
	StartLine int        `json:"start_line"`
	EndLine   int        `json:"end_line"`
	TotalLine int        `json:"total_lines"`
	Lines     []LineText `json:"lines"`
}

type LineText struct {
	Number int    `json:"number"`
	Text   string `json:"text"`
}

func (workspace *Workspace) ReadFile(path string, startLine, endLine int) (ReadResult, error) {
	info, resolved, err := workspace.statRegularFile(path)
	if err != nil {
		return ReadResult{}, err
	}
	if info.Size() > workspace.Limits.MaxFileSizeBytes {
		return ReadResult{}, fmt.Errorf("file %q exceeds max size of %d bytes", path, workspace.Limits.MaxFileSizeBytes)
	}
	contents, err := os.ReadFile(resolved)
	if err != nil {
		return ReadResult{}, fmt.Errorf("read %q: %w", path, err)
	}
	if !isText(contents) {
		return ReadResult{}, fmt.Errorf("file %q appears to be binary or non-UTF-8 text", path)
	}
	if len(contents) > workspace.Limits.MaxOutputBytes {
		contents = contents[:workspace.Limits.MaxOutputBytes]
	}
	lines := splitLines(contents)
	total := len(lines)
	if startLine <= 0 {
		startLine = 1
	}
	if endLine <= 0 || endLine > total {
		endLine = total
	}
	if startLine > endLine {
		return ReadResult{}, fmt.Errorf("invalid line range %d-%d", startLine, endLine)
	}
	resultLines := make([]LineText, 0, len(lines))
	for line := startLine; line <= endLine; line++ {
		resultLines = append(resultLines, LineText{Number: line, Text: lines[line-1]})
	}
	return ReadResult{
		Path:      relativeFromRoot(workspace.Root, resolved),
		StartLine: startLine,
		EndLine:   endLine,
		TotalLine: total,
		Lines:     resultLines,
	}, nil
}

func (workspace *Workspace) StatFile(path string) (FileInfo, error) {
	info, resolved, err := workspace.statRegularFile(path)
	if err != nil {
		return FileInfo{}, err
	}
	return FileInfo{Path: relativeFromRoot(workspace.Root, resolved), Size: info.Size()}, nil
}

func splitLines(contents []byte) []string {
	if len(contents) == 0 {
		return []string{}
	}
	text := strings.ReplaceAll(string(contents), "\r\n", "\n")
	parts := strings.Split(text, "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

func isText(contents []byte) bool {
	if bytes.IndexByte(contents, 0) >= 0 {
		return false
	}
	return utf8.Valid(contents)
}

type SearchOptions struct {
	Query      string
	Path       string
	MaxResults int
}

type SearchMatch struct {
	Path       string `json:"path"`
	LineNumber int    `json:"line_number"`
	Line       string `json:"line"`
}

type SearchResult struct {
	Matches   []SearchMatch `json:"matches"`
	Truncated bool          `json:"truncated,omitempty"`
	Skipped   []string      `json:"skipped,omitempty"`
}

func (workspace *Workspace) SearchFiles(options SearchOptions) (SearchResult, error) {
	if strings.TrimSpace(options.Query) == "" {
		return SearchResult{}, errors.New("query is required")
	}
	maxResults := options.MaxResults
	if maxResults <= 0 || maxResults > workspace.Limits.MaxSearchResults {
		maxResults = workspace.Limits.MaxSearchResults
	}
	startPath, err := workspace.ResolveExistingPath(options.Path)
	if err != nil {
		return SearchResult{}, err
	}
	result := SearchResult{Matches: make([]SearchMatch, 0, maxResults)}
	walkErr := filepath.WalkDir(startPath, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			result.Skipped = append(result.Skipped, fmt.Sprintf("%s: %v", relativeFromRoot(workspace.Root, path), walkErr))
			return nil
		}
		if entry.IsDir() {
			depth := pathDepth(relativeFromRoot(startPath, path))
			if depth > workspace.Limits.MaxTraversalDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if len(result.Matches) >= maxResults {
			result.Truncated = true
			return io.EOF
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			result.Skipped = append(result.Skipped, fmt.Sprintf("%s: %v", relativeFromRoot(workspace.Root, path), infoErr))
			return nil
		}
		if !info.Mode().IsRegular() {
			result.Skipped = append(result.Skipped, fmt.Sprintf("%s: unsupported file type", relativeFromRoot(workspace.Root, path)))
			return nil
		}
		if info.Size() > workspace.Limits.MaxFileSizeBytes {
			result.Skipped = append(result.Skipped, fmt.Sprintf("%s: file too large", relativeFromRoot(workspace.Root, path)))
			return nil
		}
		matches, skipped, matchErr := searchFile(path, options.Query, maxResults-len(result.Matches))
		if skipped {
			result.Skipped = append(result.Skipped, fmt.Sprintf("%s: non-text file", relativeFromRoot(workspace.Root, path)))
			return nil
		}
		if matchErr != nil {
			result.Skipped = append(result.Skipped, fmt.Sprintf("%s: %v", relativeFromRoot(workspace.Root, path), matchErr))
			return nil
		}
		for _, match := range matches {
			match.Path = relativeFromRoot(workspace.Root, path)
			result.Matches = append(result.Matches, match)
			if len(result.Matches) >= maxResults {
				result.Truncated = true
				return io.EOF
			}
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, io.EOF) {
		return SearchResult{}, walkErr
	}
	sort.Slice(result.Matches, func(i, j int) bool {
		if result.Matches[i].Path == result.Matches[j].Path {
			return result.Matches[i].LineNumber < result.Matches[j].LineNumber
		}
		return result.Matches[i].Path < result.Matches[j].Path
	})
	sort.Strings(result.Skipped)
	return result, nil
}

func searchFile(path, query string, remaining int) ([]SearchMatch, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	probe := make([]byte, 4096)
	readCount, err := file.Read(probe)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	probe = probe[:readCount]
	if !isText(probe) {
		return nil, true, nil
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, false, err
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNumber := 0
	matches := make([]SearchMatch, 0)
	for scanner.Scan() {
		lineNumber++
		text := scanner.Text()
		if strings.Contains(text, query) {
			matches = append(matches, SearchMatch{LineNumber: lineNumber, Line: text})
			if len(matches) >= remaining {
				return matches, false, nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, false, err
	}
	return matches, false, nil
}

func pathDepth(rel string) int {
	if rel == "." || rel == "" {
		return 0
	}
	return strings.Count(rel, string(os.PathSeparator)) + 1
}

func (workspace *Workspace) WriteFile(path, content string) error {
	resolved, err := workspace.ResolvePath(path)
	if err != nil {
		return err
	}
	if int64(len(content)) > workspace.Limits.MaxFileSizeBytes {
		return fmt.Errorf("content exceeds max size of %d bytes", workspace.Limits.MaxFileSizeBytes)
	}
	if info, statErr := os.Lstat(resolved); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to write through symlink path %q", path)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("path %q is not a regular file", path)
		}
	}
	parent := filepath.Dir(resolved)
	parentInfo, statErr := os.Stat(parent)
	if statErr != nil {
		return fmt.Errorf("parent directory %q does not exist", relativeFromRoot(workspace.Root, parent))
	}
	if !parentInfo.IsDir() {
		return fmt.Errorf("parent path %q is not a directory", relativeFromRoot(workspace.Root, parent))
	}
	tmpFile, err := os.CreateTemp(parent, ".groovy-agent-write-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmpFile.WriteString(content); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, resolved); err != nil {
		return fmt.Errorf("replace %q: %w", path, err)
	}
	removeTmp = false
	return nil
}

func (workspace *Workspace) statRegularFile(path string) (os.FileInfo, string, error) {
	resolved, err := workspace.ResolveExistingPath(path)
	if err != nil {
		return nil, "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, "", fmt.Errorf("stat %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("path %q is not a regular file", path)
	}
	return info, resolved, nil
}

func (workspace *Workspace) Mkdir(path string) error {
	resolved, err := workspace.ResolvePath(path)
	if err != nil {
		return err
	}
	if resolved == workspace.Root {
		return nil
	}
	if err := os.MkdirAll(resolved, 0o755); err != nil {
		return fmt.Errorf("create directory %q: %w", path, err)
	}
	return nil
}
