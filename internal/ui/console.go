package ui

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

const (
	ansiRed    = "\x1b[1;31m"
	ansiGreen  = "\x1b[1;32m"
	ansiYellow = "\x1b[1;33m"
	ansiCyan   = "\x1b[36m"
	ansiDim    = "\x1b[2m"
	ansiReset  = "\x1b[0m"
)

// Console writes safe diagnostics and adds ANSI styling only on an interactive terminal.
type Console struct {
	out   io.Writer
	color bool
}

func NewConsole(out io.Writer) *Console {
	return &Console{out: out, color: colorEnabled(out)}
}

func terminalEnabled(out io.Writer) bool {
	f, ok := out.(*os.File)
	return ok && term.IsTerminal(int(f.Fd())) && os.Getenv("TERM") != "dumb"
}

func colorEnabled(out io.Writer) bool {
	f, ok := out.(*os.File)
	terminal := ok && term.IsTerminal(int(f.Fd()))
	_, noColor := os.LookupEnv("NO_COLOR")
	return colorAllowed(terminal, noColor, os.Getenv("TERM"))
}

func colorAllowed(terminal, noColor bool, termName string) bool {
	return terminal && !noColor && termName != "dumb"
}

func paint(enabled bool, code, text string) string {
	if !enabled || text == "" {
		return text
	}
	return code + text + ansiReset
}

func (c *Console) Accent(text string) string  { return paint(c.color, ansiCyan, text) }
func (c *Console) Muted(text string) string   { return paint(c.color, ansiDim, text) }
func (c *Console) Success(text string) string { return paint(c.color, ansiGreen, text) }

func (c *Console) Warning(message string) { c.writeDiagnostic("Warning", message, ansiYellow) }
func (c *Console) Error(err error)        { c.writeDiagnostic("Error", err.Error(), ansiRed) }

func (c *Console) writeDiagnostic(label, message, color string) {
	lines := strings.Split(message, "\n")
	fmt.Fprintf(c.out, "%s %s\n", paint(c.color, color, label+":"), Safe(lines[0]))
	for _, line := range lines[1:] {
		fmt.Fprintf(c.out, "  %s\n", Safe(line))
	}
}
