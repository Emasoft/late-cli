package session

import (
	"encoding/json"
	"fmt"
	"late/internal/client"
	"os"
	"path/filepath"
)

const (
	// historyFileMode keeps session history files as private as the session
	// metadata sidecar (models.go configFilePerm).
	historyFileMode os.FileMode = 0o600
)

// writeAtomic persists data to path through a temp file + rename in the
// target directory, creating the directory first. The temp file lives in the
// destination directory (same filesystem, so the rename is atomic) and is
// removed on every failure path; the rename replaces any existing file
// whole, so readers never observe a partially written document. It is the
// shared write primitive for every session sidecar (history, manifest).
func writeAtomic(path string, data []byte, fileMode, dirMode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	tmpFile, err := os.CreateTemp(dir, "session-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name()) // Clean up if something goes wrong before rename

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write to temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}
	if err := os.Chmod(tmpFile.Name(), fileMode); err != nil {
		return fmt.Errorf("failed to set file mode: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tmpFile.Name(), path); err != nil {
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
}

// SaveHistory atomically saves the chat history to the specified path.
func SaveHistory(path string, history []client.ChatMessage) error {
	if path == "" {
		return nil // Skip saving if no path provided
	}

	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal history: %w", err)
	}

	if err := writeAtomic(path, data, historyFileMode, 0700); err != nil {
		return fmt.Errorf("failed to save history: %w", err)
	}

	return nil
}

// LoadHistory loads the chat history from the specified path.
func LoadHistory(path string) ([]client.ChatMessage, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return []client.ChatMessage{}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read history file: %w", err)
	}

	var history []client.ChatMessage
	if err := json.Unmarshal(data, &history); err != nil {
		return nil, fmt.Errorf("failed to unmarshal history: %w", err)
	}

	return history, nil
}
