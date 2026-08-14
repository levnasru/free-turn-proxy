package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

// selectFromKeys reads raw key bytes from `keys` (already in whatever mode
// the caller set up — this function does no terminal-mode switching itself,
// which is what makes it testable with a plain strings.Reader) and returns
// the selected index once Enter/'\r'/'\n' arrives. Ctrl+C ('\x03') returns
// an error so the caller can exit cleanly instead of looping forever.
func selectFromKeys(items []string, keys io.Reader, out io.Writer) (int, error) {
	sel := 0
	render := func() {
		fmt.Fprint(out, "\r\n")
		for i, it := range items {
			marker := "  "
			if i == sel {
				marker = "> "
			}
			fmt.Fprintf(out, "%s%s\r\n", marker, it)
		}
	}
	render()

	buf := make([]byte, 3)
	for {
		n, err := keys.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0, errors.New("selectFromKeys: input closed before selection")
			}
			return 0, err
		}
		chunk := buf[:n]
		switch {
		case n == 1 && chunk[0] == 0x03:
			return 0, errors.New("selectFromKeys: interrupted")
		case n == 1 && (chunk[0] == '\r' || chunk[0] == '\n'):
			return sel, nil
		case n == 3 && chunk[0] == 0x1b && chunk[1] == '[' && chunk[2] == 'A': // up
			sel = (sel - 1 + len(items)) % len(items)
			render()
		case n == 3 && chunk[0] == 0x1b && chunk[1] == '[' && chunk[2] == 'B': // down
			sel = (sel + 1) % len(items)
			render()
		}
	}
}

// RunMenu puts stdin into raw mode, runs selectFromKeys against it, restores
// the terminal, and returns the selected item text (not just its index — the
// caller in main.go switches on the string).
func RunMenu(items []string) (string, error) {
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return "", fmt.Errorf("raw mode: %w", err)
	}
	defer term.Restore(fd, oldState)

	idx, err := selectFromKeys(items, os.Stdin, os.Stdout)
	if err != nil {
		return "", err
	}
	return items[idx], nil
}
