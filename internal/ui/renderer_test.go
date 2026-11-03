package ui

import (
	"bytes"
	"fmt"
	"gobackupper/internal/backup"
	"strings"
	"testing"
	"time"
)

func TestSafeRejectsTerminalControlsAndInvalidUTF8(t *testing.T) {
	input := "file\x1b[2J\n\r\t\x00" + string([]byte{0xff}) + " кириллица \"quoted\""
	output := Safe(input)
	if strings.ContainsAny(output, "\x1b\n\r\t\x00") {
		t.Fatalf("control injection: %q", output)
	}
	if !strings.Contains(output, "\\x1b") || !strings.Contains(output, "\\xff") || !strings.Contains(output, "кириллица") {
		t.Fatalf("lost safe display: %q", output)
	}
	if !strings.Contains(output, `"quoted"`) {
		t.Fatalf("ordinary quotes were escaped: %q", output)
	}
}

func TestConsoleFormatsMultilineDiagnosticsWithoutANSIForRedirectedOutput(t *testing.T) {
	var out bytes.Buffer
	NewConsole(&out).Warning("cannot verify workspace\nbtrfs subvolume show failed\nOutput:\n  ERROR: permission denied")
	want := "Warning: cannot verify workspace\n  btrfs subvolume show failed\n  Output:\n    ERROR: permission denied\n"
	if out.String() != want {
		t.Fatalf("diagnostic output:\n%s\nwant:\n%s", out.String(), want)
	}
	if strings.ContainsRune(out.String(), '\x1b') {
		t.Fatal("redirected output contains ANSI")
	}
}

func TestConsoleAppliesANSIStylesWhenColorIsEnabled(t *testing.T) {
	var out bytes.Buffer
	console := &Console{out: &out, color: true}
	console.Warning("warning")
	console.Error(fmt.Errorf("failure"))
	if got := out.String(); !strings.Contains(got, "\x1b[1;33mWarning:\x1b[0m") ||
		!strings.Contains(got, "\x1b[1;31mError:\x1b[0m") {
		t.Fatalf("missing warning/error ANSI styles: %q", got)
	}
	if got := console.Success("done"); !strings.Contains(got, "\x1b[1;32m") {
		t.Fatalf("missing success ANSI style: %q", got)
	}
}

func TestColorAllowedRequiresInteractiveCapableTerminal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		terminal bool
		noColor  bool
		termName string
		want     bool
	}{
		{"interactive", true, false, "xterm", true},
		{"redirected", false, false, "xterm", false},
		{"no-color", true, true, "xterm", false},
		{"dumb-terminal", true, false, "dumb", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := colorAllowed(tc.terminal, tc.noColor, tc.termName); got != tc.want {
				t.Fatalf("colorAllowed=%t, want %t", got, tc.want)
			}
		})
	}
}

func TestPlainOutputDoesNotPretendProgressMeansSuccess(t *testing.T) {
	var out bytes.Buffer
	r := New(&out)
	job := backup.Job{Name: "Documents"}
	r.Event(backup.Event{Stage: backup.StageSync, Job: job})
	r.Progress(job, backup.Progress{HasProgress: true, Percent: 100, TotalKnown: true, Current: "file\nname"})
	r.render()
	if strings.Contains(out.String(), "Saved") || strings.ContainsRune(out.String(), '\x1b') {
		t.Fatalf("false success or ANSI: %q", out.String())
	}
	if !strings.Contains(out.String(), "file\\nname") {
		t.Fatalf("unsafe filename: %q", out.String())
	}
	r.Event(backup.Event{Stage: backup.StageSuccess, Job: job, Snapshot: backup.Snapshot{Path: "/snapshots/day", PreviousPath: "/snapshots/day.0"}})
	if !strings.Contains(out.String(), "Saved Documents: /snapshots/day") || !strings.Contains(out.String(), "Previous snapshot: /snapshots/day.0") {
		t.Fatalf("missing storage result: %q", out.String())
	}
}

type blockingOutput struct {
	entered chan struct{}
	release chan struct{}
}

func (w blockingOutput) Write(p []byte) (int, error) {
	w.entered <- struct{}{}
	<-w.release
	return len(p), nil
}

func TestSlowOutputDoesNotBlockProgressReader(t *testing.T) {
	writer := blockingOutput{entered: make(chan struct{}, 8), release: make(chan struct{})}
	r := New(writer)
	job := backup.Job{Name: "A"}
	done := make(chan struct{})
	go func() { defer close(done); r.Event(backup.Event{Stage: backup.StageSync, Job: job}) }()
	<-writer.entered
	updated := make(chan struct{})
	go func() { r.Progress(job, backup.Progress{Bytes: 123}); close(updated) }()
	select {
	case <-updated:
	case <-time.After(time.Second):
		close(writer.release)
		<-done
		t.Fatal("reader blocked on UI output")
	}
	close(writer.release)
	<-done
	r.mu.Lock()
	state := r.state
	r.mu.Unlock()
	if state.progress.Bytes != 123 {
		t.Fatalf("progress lost: %+v", state)
	}
}

func TestPhaseTransitionDiscardsPreviouslyComputedProgress(t *testing.T) {
	var out bytes.Buffer
	r := New(&out)
	job := backup.Job{Name: "A"}
	r.Event(backup.Event{Stage: backup.StageSync, Job: job})
	r.mu.Lock()
	stale := r.state
	r.mu.Unlock()
	r.Event(backup.Event{Stage: backup.StageSnapshot, Job: job})
	before := out.String()
	r.writeProgress(stale, "outdated progress")
	if out.String() != before {
		t.Fatalf("old sync frame overwrote phase: %q", out.String())
	}
}
