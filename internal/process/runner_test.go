package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunDrainsAndBoundsBothStreams(t *testing.T) {
	r := Runner{}
	res, err := r.Run(context.Background(), "sh", []string{"-c", "head -c 200000 /dev/zero | tr '\\000' o; head -c 200000 /dev/zero | tr '\\000' e >&2; printf FINAL"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) != outputLimit || len(res.Stderr) != outputLimit || !res.Truncated {
		t.Fatalf("unexpected bounded result: stdout=%d stderr=%d truncated=%v", len(res.Stdout), len(res.Stderr), res.Truncated)
	}
	if !strings.HasSuffix(res.Stdout, "FINAL") || !strings.HasSuffix(res.Stderr, strings.Repeat("e", 64)) {
		t.Fatal("final output was not retained")
	}
}
func TestRunReturnsStatusAndCause(t *testing.T) {
	_, err := (Runner{}).Run(context.Background(), "sh", []string{"-c", "printf tail >&2; exit 7"}, nil)
	var runErr *Error
	if !errors.As(err, &runErr) || runErr.Result.ExitCode != 7 || !strings.Contains(runErr.Result.Stderr, "tail") {
		t.Fatalf("error lacks exit status or diagnostic: %#v", err)
	}
}

func TestErrorFormatsCommandAndDiagnosticsOnSeparateLines(t *testing.T) {
	err := &Error{
		Name:   "btrfs",
		Args:   []string{"subvolume", "show", "/some storage"},
		Result: Result{ExitCode: 1, Stderr: "ERROR: permission denied\nadditional detail\n"},
		Err:    errors.New("exit status 1"),
	}
	want := "btrfs subvolume show \"/some storage\" failed (exit 1)\nOutput:\n  ERROR: permission denied\n  additional detail"
	if got := err.Error(); got != want {
		t.Fatalf("formatted error:\n%s\nwant:\n%s", got, want)
	}
}
func TestRunCancellationKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	ready := make(chan int, 1)
	go func() {
		_, err := (Runner{GracePeriod: 80 * time.Millisecond, WaitDelay: 100 * time.Millisecond}).Run(ctx, "sh", []string{"-c", "sh -c 'trap \"\" TERM; echo $$; exec sleep 30 >/dev/null 2>&1' </dev/null & wait"}, pidReporter{ready})
		done <- err
	}()
	var pid int
	select {
	case pid = <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("child never became ready")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not reap process group")
	}
	// SIGKILL delivery is asynchronous; allow the child to be scheduled before
	// checking that it exited. It is not our direct child to reap.
	deadline := time.Now().Add(time.Second)
	for running(pid) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if running(pid) {
		t.Fatalf("TERM-ignoring descendant %d survived", pid)
	}
}

type pidReporter struct{ ready chan<- int }

func (w pidReporter) Write(p []byte) (int, error) {
	if pid, err := strconv.Atoi(strings.TrimSpace(string(p))); err == nil {
		select {
		case w.ready <- pid:
		default:
		}
	}
	return len(p), nil
}
func running(pid int) bool {
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false
	}
	b, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if e != nil {
		return false
	}
	f := strings.Fields(string(b))
	return len(f) > 2 && f[2] != "Z"
}
