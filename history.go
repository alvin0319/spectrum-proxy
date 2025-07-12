package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// HistoryFile manages the command history stored in a file
type HistoryFile struct {
	filePath string
	maxSize  int
	history  []string
}

// NewHistoryFile creates a new history file manager
func NewHistoryFile(filename string, maxSize int) (*HistoryFile, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	spectrumDir := filepath.Join(homeDir, ".spectrum")
	if _, err := os.Stat(spectrumDir); os.IsNotExist(err) {
		if err := os.Mkdir(spectrumDir, 0755); err != nil {
			return nil, err
		}
	}

	filePath := filepath.Join(spectrumDir, filename)
	h := &HistoryFile{
		filePath: filePath,
		maxSize:  maxSize,
		history:  []string{},
	}

	if err := h.Load(); err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	return h, nil
}

// Load reads history from the file
func (h *HistoryFile) Load() error {
	file, err := os.Open(h.filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var lines []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}

	// Only keep the last maxSize entries
	if len(lines) > h.maxSize {
		lines = lines[len(lines)-h.maxSize:]
	}

	h.history = lines
	return scanner.Err()
}

// Save writes history to the file
func (h *HistoryFile) Save() error {
	file, err := os.Create(h.filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	for _, line := range h.history {
		if _, err := fmt.Fprintln(writer, line); err != nil {
			return err
		}
	}
	return writer.Flush()
}

// Append adds a command to history
func (h *HistoryFile) Append(cmd string) error {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return nil
	}

	// Don't add duplicate of the last command
	if len(h.history) > 0 && h.history[len(h.history)-1] == cmd {
		return nil
	}

	h.history = append(h.history, cmd)

	// Trim history if it exceeds maxSize
	if len(h.history) > h.maxSize {
		h.history = h.history[len(h.history)-h.maxSize:]
	}

	return h.Save()
}

// GetHistory returns the history as a slice
func (h *HistoryFile) GetHistory() []string {
	return h.history
}
