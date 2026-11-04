// Shared selection frame: framed arrow-key menu with a dot marker.
// Copy unchanged; needs printer from cli.go.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/term"
)

// Marker dot of the current item
const menuDot = "🟢"

// Pick one item with the arrow keys; false when cancelled or stdin is no terminal
func (p printer) choose(ctx context.Context, title string, items []string) (int, bool) {
	descriptor := int(os.Stdin.Fd())
	state, err := term.MakeRaw(descriptor)
	if err != nil {
		return 0, false
	}
	fmt.Fprint(p.out, "\033[?25l")
	defer func() {
		fmt.Fprint(p.out, "\033[?25h")
		term.Restore(descriptor, state)
	}()

	paint := func(text, style string) string {
		if p.color {
			return style + text + "\033[0m"
		}
		return text
	}
	// Frame tinted line by line; raw mode needs explicit carriage returns
	drawn := 0
	row := func(left, text string, used int, right string) {
		shade := tint(drawn, len(items)+3)
		drawn = (drawn + 1) % (len(items) + 4)
		fmt.Fprint(p.out, "\r\033[K"+paint(left, shade)+text+paint(strings.Repeat("─", max(0, used))+right, shade)+"\r\n")
	}
	// Inner width fits the title, the widest item behind its dot, and the footer
	digits := len(fmt.Sprint(len(items)))
	width := max(cellWidth(title), cellWidth(" / · ↑↓ · Enter · q ")+2*digits-2)
	for _, item := range items {
		width = max(width, cellWidth(item)+3)
	}
	line := func(text string, cells int) {
		row("│  ", text+strings.Repeat(" ", width-cells+2), 0, "│")
	}
	keys := make(chan string, 1)
	selected := 0
	for {
		row("╭", "", width+4, "╮")
		line(paint(title, "\033[1m"), cellWidth(title))
		row("├", "", width+4, "┤")
		for index, item := range items {
			// The dot marks the current item; the others fade
			if index == selected {
				line(menuDot+" "+paint(item, "\033[1m"), cellWidth(item)+3)
			} else {
				line("⚪ "+paint(item, "\033[2m"), cellWidth(item)+3)
			}
		}
		footer := fmt.Sprintf(" %*d/%d · ↑↓ · Enter · q ", digits, selected+1, len(items))
		row("╰─", footer, width+3-cellWidth(footer), "╯")

		// Key read in the background so a signal still cancels
		go func() {
			buffer := make([]byte, 8)
			count, err := os.Stdin.Read(buffer)
			if err != nil {
				keys <- "q"
				return
			}
			keys <- string(buffer[:count])
		}()
		key := "q"
		select {
		case <-ctx.Done():
		case key = <-keys:
		}

		// Back to the top border for the redraw or the cleanup
		fmt.Fprintf(p.out, "\033[%dA", len(items)+4)
		switch key {
		case "\033[A", "\033OA", "k":
			selected = (selected + len(items) - 1) % len(items)
		case "\033[B", "\033OB", "j":
			selected = (selected + 1) % len(items)
		case "\r", "\n":
			fmt.Fprint(p.out, "\033[J")
			return selected, true
		case "\x03":
			// Raw mode swallows Ctrl-C; hand it to the frame's signal context
			syscall.Kill(syscall.Getpid(), syscall.SIGINT)
			<-ctx.Done()
			fallthrough
		case "q", "\033":
			fmt.Fprint(p.out, "\033[J")
			return 0, false
		}
	}
}

// Terminal cells of a text; emojis take two
func cellWidth(text string) int {
	width := 0
	for _, character := range text {
		width++
		if character >= 0x1F000 {
			width++
		}
	}
	return width
}
