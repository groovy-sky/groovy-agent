package gittools

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestStatusAndDiff(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		command := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-m", "init")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, err := Status(repo, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if status == "" {
		t.Fatalf("expected status output")
	}
	diff, err := Diff(repo, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if diff == "" {
		t.Fatalf("expected diff output")
	}
}
