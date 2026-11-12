// tuiwhatsapp — a WhatsApp client for the terminal.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
)

func dataDir() (string, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "share")
	}
	dir := filepath.Join(base, "tuiwhatsapp")
	return dir, os.MkdirAll(dir, 0o700)
}

func run() error {
	dir, err := dataDir()
	if err != nil {
		return err
	}
	st, err := OpenStore(filepath.Join(dir, "messages.db"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wa, err := NewWA(ctx, dir, st)
	if err != nil {
		return err
	}
	p := tea.NewProgram(newModel(wa, st), tea.WithAltScreen(), tea.WithMouseCellMotion())
	wa.send = p.Send
	go wa.Start()
	_, err = p.Run()
	wa.Stop()
	return err
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "tuiwhatsapp:", err)
		os.Exit(1)
	}
}
