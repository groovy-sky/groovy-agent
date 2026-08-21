package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvePathRejectsTraversalAndOutsideAbsolute(t *testing.T) {
	root := t.TempDir()
	workspace, err := New(root, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.ResolvePath("../outside.txt"); err == nil {
		t.Fatal("expected traversal error")
	}
	outside := filepath.Join(filepath.Dir(root), "outside.txt")
	if _, err := workspace.ResolvePath(outside); err == nil {
		t.Fatal("expected absolute outside path error")
	}
}

func TestNormalizeRelativePathRejectsAbsoluteAndTraversal(t *testing.T) {
	root := t.TempDir()
	workspace, err := New(root, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.NormalizeRelativePath("../outside.txt"); err == nil {
		t.Fatal("expected traversal error")
	}
	if _, err := workspace.NormalizeRelativePath(filepath.Join(root, "inside.txt")); err == nil {
		t.Fatal("expected absolute path error")
	}
}

func TestNormalizeRelativePathCleansWorkspaceRelativePath(t *testing.T) {
	root := t.TempDir()
	workspace, err := New(root, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	got, err := workspace.NormalizeRelativePath("notes/../notes/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got != "notes/a.txt" {
		t.Fatalf("normalized path = %q", got)
	}
}

func TestResolvePathRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outsideDir := t.TempDir()
	if err := os.Symlink(outsideDir, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}
	workspace, err := New(root, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.ResolvePath("link/file.txt"); err == nil {
		t.Fatal("expected symlink escape error")
	}
}

func TestWriteReadSearchAndList(t *testing.T) {
	root := t.TempDir()
	workspace, err := New(root, Limits{MaxTraversalDepth: 4, MaxSearchResults: 10, MaxListEntries: 20, MaxOutputBytes: 4096, MaxFileSizeBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.Mkdir("notes/sub"); err != nil {
		t.Fatal(err)
	}
	if err := workspace.WriteFile("notes/sub/a.txt", "hello\nworld\n"); err != nil {
		t.Fatal(err)
	}
	list, err := workspace.ListFiles(ListOptions{Path: "notes", Depth: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Entries) == 0 {
		t.Fatalf("expected entries in list: %+v", list)
	}

	read, err := workspace.ReadFile("notes/sub/a.txt", 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Lines) != 1 || read.Lines[0].Text != "world" {
		t.Fatalf("unexpected read result: %+v", read)
	}
	search, err := workspace.SearchFiles(SearchOptions{Query: "hello", Path: "notes"})
	if err != nil {
		t.Fatal(err)
	}
	if len(search.Matches) != 1 || !strings.Contains(search.Matches[0].Path, "a.txt") {
		t.Fatalf("unexpected search result: %+v", search)
	}
}

func TestReadFileRejectsBinary(t *testing.T) {
	root := t.TempDir()
	workspace, err := New(root, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "bin.dat")
	if err := os.WriteFile(path, []byte{'h', 0, 'i'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.ReadFile("bin.dat", 0, 0); err == nil {
		t.Fatal("expected binary file error")
	}
}

func TestStatFileRejectsNonRegularFiles(t *testing.T) {
	root := t.TempDir()
	workspace, err := New(root, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.Mkdir("notes"); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.StatFile("notes"); err == nil {
		t.Fatal("expected directory stat error")
	}
}
