package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	SessionsDir                = ".groovy-agent/sessions"
	DefaultMaxInstructionBytes = 32 << 10
)

type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

type Snapshot struct {
	Type      string    `json:"type"`
	SessionID string    `json:"session_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Messages  []Message `json:"messages"`
}

type Store struct {
	WorkspaceRoot       string
	MaxInstructionBytes int
}

func NewStore(workspaceRoot string) *Store {
	return &Store{WorkspaceRoot: workspaceRoot, MaxInstructionBytes: DefaultMaxInstructionBytes}
}

func NewSessionID(now time.Time) string {
	return now.UTC().Format("20060102T150405.000000000")
}

func (store *Store) SaveSnapshot(sessionID string, messages []Message, createdAt, updatedAt time.Time) error {
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("session id is required")
	}
	sessionsPath := filepath.Join(store.WorkspaceRoot, SessionsDir)
	if err := os.MkdirAll(sessionsPath, 0o755); err != nil {
		return fmt.Errorf("create sessions directory: %w", err)
	}
	filePath := filepath.Join(sessionsPath, sessionID+".jsonl")
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open session file: %w", err)
	}
	defer file.Close()
	snapshot := Snapshot{
		Type:      "snapshot",
		SessionID: sessionID,
		CreatedAt: createdAt.UTC(),
		UpdatedAt: updatedAt.UTC(),
		Messages:  messages,
	}
	if err := json.NewEncoder(file).Encode(snapshot); err != nil {
		return fmt.Errorf("write session snapshot: %w", err)
	}
	return nil
}

func (store *Store) LoadLatestSnapshot(sessionID string) (Snapshot, error) {
	path := filepath.Join(store.WorkspaceRoot, SessionsDir, sessionID+".jsonl")
	file, err := os.Open(path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("open session file: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	var latest Snapshot
	found := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var snap Snapshot
		if err := json.Unmarshal([]byte(line), &snap); err != nil {
			continue
		}
		if snap.Type != "snapshot" {
			continue
		}
		latest = snap
		found = true
	}
	if err := scanner.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("read session file: %w", err)
	}
	if !found {
		return Snapshot{}, fmt.Errorf("no snapshots found for session %q", sessionID)
	}
	return latest, nil
}

func (store *Store) LoadProjectInstructions() (string, []string, error) {
	maxBytes := store.MaxInstructionBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxInstructionBytes
	}
	paths := []string{"GROOVY.md", "AGENTS.md", ".groovy-agent/instructions.md"}
	remaining := maxBytes
	var parts []string
	var sources []string
	for _, rel := range paths {
		abs := filepath.Join(store.WorkspaceRoot, rel)
		content, err := os.ReadFile(abs)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", nil, fmt.Errorf("read instructions file %s: %w", rel, err)
		}
		if len(content) > remaining {
			content = content[:remaining]
		}
		if len(content) == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("[%s]\n%s", rel, strings.TrimSpace(string(content))))
		sources = append(sources, rel)
		remaining -= len(content)
		if remaining <= 0 {
			break
		}
	}
	return strings.Join(parts, "\n\n"), sources, nil
}
