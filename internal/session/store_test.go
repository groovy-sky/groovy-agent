package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveAndLoadLatestSnapshot(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	messages := []Message{{Role: "user", Content: mustRaw(t, "hello")}}
	created := time.Now().Add(-time.Minute)
	updated := time.Now()
	if err := store.SaveSnapshot("s1", messages, created, updated); err != nil {
		t.Fatal(err)
	}
	messages2 := []Message{{Role: "assistant", Content: mustRaw(t, "done")}}
	if err := store.SaveSnapshot("s1", messages2, created, updated.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.LoadLatestSnapshot("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Messages) != 1 || snapshot.Messages[0].Role != "assistant" {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
}

func TestLoadProjectInstructionsOrderAndBounds(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "GROOVY.md"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".groovy-agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".groovy-agent", "instructions.md"), []byte("third"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewStore(root)
	store.MaxInstructionBytes = 20
	text, sources, err := store.LoadProjectInstructions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 3 || sources[0] != "GROOVY.md" || sources[1] != "AGENTS.md" || sources[2] != ".groovy-agent/instructions.md" {
		t.Fatalf("unexpected sources: %+v", sources)
	}
	if !strings.Contains(text, "[GROOVY.md]") || !strings.Contains(text, "[AGENTS.md]") {
		t.Fatalf("unexpected text: %q", text)
	}
}

func mustRaw(t *testing.T, text string) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
