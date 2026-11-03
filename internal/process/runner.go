// Package process owns external command lifetimes and bounded diagnostics.
package process

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

const outputLimit = 64 << 10

type Result struct {
	Stdout, Stderr string
	ExitCode       int
	Truncated      bool
}
type Error struct {
	Name   string
	Args   []string
	Result Result
	Err    error
}

func (e *Error) Error() string {
	command := e.Name
	if len(e.Args) > 0 {
		command += " " + displayArgs(e.Args)
	}
	lines := []string{fmt.Sprintf("%s failed (exit %d)", command, e.Result.ExitCode)}
	if e.Err != nil && e.Err.Error() != fmt.Sprintf("exit status %d", e.Result.ExitCode) {
		lines = append(lines, "Cause: "+e.Err.Error())
	}
	diagnostic := e.Result.Stderr
	if diagnostic == "" {
		diagnostic = e.Result.Stdout
	}
	if diagnostic != "" {
		title := "Output:"
		if e.Result.Truncated {
			title = "Output (truncated):"
		}
		lines = append(lines, title)
		for _, line := range strings.Split(strings.TrimSuffix(diagnostic, "\n"), "\n") {
			lines = append(lines, "  "+line)
		}
	}
	return strings.Join(lines, "\n")
}
func displayArgs(args []string) string {
	var rendered strings.Builder
	for i, arg := range args {
		if i > 0 {
			rendered.WriteByte(' ')
		}
		if arg == "" || strings.IndexFunc(arg, func(r rune) bool {
			return unicode.IsSpace(r) || unicode.IsControl(r) || r == '"' || r == '\\'
		}) >= 0 {
			rendered.WriteString(strconv.Quote(arg))
		} else {
			rendered.WriteString(arg)
		}
	}
	return rendered.String()
}

func (e *Error) Unwrap() error { return e.Err }

type Runner struct{ GracePeriod, WaitDelay time.Duration }
type tail struct {
	sync.Mutex
	b         []byte
	truncated bool
}

func (t *tail) Write(p []byte) (int, error) {
	t.Lock()
	defer t.Unlock()
	n := len(p)
	if len(p) >= outputLimit {
		t.truncated = t.truncated || len(t.b) != 0 || len(p) > outputLimit
		t.b = append(t.b[:0], p[len(p)-outputLimit:]...)
		return n, nil
	}
	overflow := len(t.b) + len(p) - outputLimit
	if overflow > 0 {
		copy(t.b, t.b[overflow:])
		t.b = t.b[:len(t.b)-overflow]
		t.truncated = true
	}
	t.b = append(t.b, p...)
	return n, nil
}
func (t *tail) value() (string, bool) { t.Lock(); defer t.Unlock(); return string(t.b), t.truncated }

// Run drains both streams into bounded tails. A non-nil stdout receives stdout bytes
// synchronously and must not block; nil captures stdout. LC_ALL is fixed for parsing.
func (r Runner) Run(ctx context.Context, name string, args []string, stdout io.Writer) (Result, error) {
	grace := r.GracePeriod
	if grace <= 0 {
		grace = time.Second
	}
	wait := r.WaitDelay
	if wait <= 0 {
		wait = 2 * time.Second
	}
	cmd := exec.Command(name, args...)
	cmd.Env = append(cmd.Environ(), "LC_ALL=C")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = wait
	var out, errout tail
	if stdout == nil {
		cmd.Stdout = &out
	} else {
		cmd.Stdout = io.MultiWriter(&out, stdout)
	}
	cmd.Stderr = &errout
	if err := ctx.Err(); err != nil {
		res := Result{ExitCode: -1}
		return res, &Error{Name: name, Args: append([]string(nil), args...), Result: res, Err: err}
	}
	if err := cmd.Start(); err != nil {
		res := Result{ExitCode: -1}
		return res, &Error{Name: name, Args: append([]string(nil), args...), Result: res, Err: err}
	}
	// Observe the leader without reaping it: its PID/PGID cannot be reused
	// during TERM escalation, even when it exits before its descendants.
	leader := make(chan error, 1)
	go func() {
		var info unix.Siginfo
		for {
			e := unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
			if e == unix.EINTR {
				continue
			}
			leader <- e
			return
		}
	}()
	var cause error
	select {
	case observeErr := <-leader:
		cause = observeErr
	case <-ctx.Done():
		cause = ctx.Err()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		timer := time.NewTimer(grace)
		<-timer.C
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cause = errors.Join(cause, <-leader)
	}
	// No descendant may outlive a completed invocation. The unreaped leader
	// still reserves the process-group ID while the final signal is sent.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	cause = errors.Join(cause, cmd.Wait())
	osout, tr1 := out.value()
	estr, tr2 := errout.value()
	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	} else {
		code = -1
	}
	res := Result{Stdout: osout, Stderr: estr, ExitCode: code, Truncated: tr1 || tr2}
	if cause != nil {
		return res, &Error{Name: name, Args: append([]string(nil), args...), Result: res, Err: cause}
	}
	return res, nil
}
