// Package ui renders latest-state telemetry without blocking subprocess readers.
package ui

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"gobackupper/internal/backup"
	"golang.org/x/term"
)

// Safe escapes controls and invalid UTF-8 while preserving ordinary quotes and backslashes.
func Safe(s string) string {
	var result strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			fmt.Fprintf(&result, "\\x%02x", s[i])
			i++
			continue
		}
		switch r {
		case '\n':
			result.WriteString("\\n")
		case '\r':
			result.WriteString("\\r")
		case '\t':
			result.WriteString("\\t")
		default:
			if unicode.IsGraphic(r) {
				result.WriteRune(r)
			} else if r <= 0xff {
				fmt.Fprintf(&result, "\\x%02x", r)
			} else if r <= 0xffff {
				fmt.Fprintf(&result, "\\u%04x", r)
			} else {
				fmt.Fprintf(&result, "\\U%08x", r)
			}
		}
		i += size
	}
	return result.String()
}

type displayState struct {
	job      string
	stage    backup.Stage
	progress backup.Progress
	started  time.Time
}

// Renderer separates cheap state updates from potentially slow output writes.
type Renderer struct {
	mu       sync.Mutex
	state    displayState
	outputMu sync.Mutex
	out      io.Writer
	terminal bool
	color    bool
	fd       int
}

func New(out io.Writer) *Renderer {
	r := &Renderer{out: out, terminal: terminalEnabled(out), color: colorEnabled(out)}
	if f, ok := out.(*os.File); ok {
		r.fd = int(f.Fd())
	}
	return r
}

func (r *Renderer) Progress(job backup.Job, progress backup.Progress) {
	r.mu.Lock()
	if r.state.job == job.Name && r.state.stage == backup.StageSync {
		r.state.progress = progress
	}
	r.mu.Unlock()
}

func (r *Renderer) Event(event backup.Event) {
	r.mu.Lock()
	r.state = displayState{job: event.Job.Name, stage: event.Stage, started: time.Now()}
	r.mu.Unlock()
	r.outputMu.Lock()
	defer r.outputMu.Unlock()
	if r.terminal {
		fmt.Fprint(r.out, "\r\x1b[2K")
	}
	if event.Stage == backup.StageSuccess {
		fmt.Fprintf(r.out, "%s %s: %s\n", paint(r.color, ansiGreen, "Saved"), Safe(event.Job.Name), Safe(event.Snapshot.Path))
		if event.Snapshot.PreviousPath != "" {
			fmt.Fprintf(r.out, "%s %s\n", paint(r.color, ansiDim, "Previous snapshot:"), Safe(event.Snapshot.PreviousPath))
		}
	} else {
		stageColor := ansiCyan
		if event.Stage == backup.StageSnapshot || event.Stage == backup.StagePrepare {
			stageColor = ansiYellow
		}
		fmt.Fprintf(r.out, "%s %s\n", paint(r.color, stageColor, string(event.Stage)+":"), Safe(event.Job.Name))
	}
}

func (r *Renderer) render() {
	r.mu.Lock()
	state := r.state
	r.mu.Unlock()
	if state.stage != backup.StageSync {
		return
	}
	p := state.progress
	elapsed := time.Since(state.started).Truncate(time.Second)
	message := fmt.Sprintf("%s: scanning/syncing; elapsed %s", Safe(state.job), elapsed)
	if p.HasProgress {
		kind := "estimate"
		if p.TotalKnown {
			kind = "rsync"
		}
		message = fmt.Sprintf("%s: %d%% (%s), %d bytes, %s; elapsed %s", Safe(state.job), p.Percent, kind, p.Bytes, Safe(p.Rate), elapsed)
	}
	if p.Current != "" {
		message += "; " + Safe(p.Current)
	}
	if p.Degraded {
		message += " [progress incomplete]"
	}
	if r.terminal {
		width, _, err := term.GetSize(r.fd)
		if err != nil || width < 2 {
			width = 80
		}
		// Truncate by bytes before UTF-8 escaping; non-ASCII is escaped when
		// width is constrained, avoiding half-runes and wide-column overflow.
		if len(message) > width-1 {
			message = strconv.QuoteToASCII(message)
			message = message[1 : len(message)-1]
			if len(message) > width-1 {
				message = message[:width-1]
			}
		}
	}
	r.writeProgress(state, message)
}

func (r *Renderer) writeProgress(state displayState, message string) {
	r.outputMu.Lock()
	defer r.outputMu.Unlock()
	// A slow render must not restore Sync after Event printed Snapshot/Success.
	r.mu.Lock()
	current := r.state.stage == state.stage && r.state.job == state.job && r.state.started.Equal(state.started)
	r.mu.Unlock()
	if !current {
		return
	}
	if r.terminal {
		fmt.Fprintf(r.out, "\r\x1b[2K%s", paint(r.color, ansiCyan, message))
	} else {
		fmt.Fprintln(r.out, message)
	}
}

// Start runs a bounded-rate display loop. The returned stop function is idempotent.
func (r *Renderer) Start(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	interval := 5 * time.Second
	if r.terminal {
		interval = 200 * time.Millisecond
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.render()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
			r.outputMu.Lock()
			defer r.outputMu.Unlock()
			if r.terminal {
				fmt.Fprint(r.out, "\r\x1b[2K")
			}
		})
	}
}
