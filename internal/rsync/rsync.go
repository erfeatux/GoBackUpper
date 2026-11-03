package rsync

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gobackupper/internal/backup"
	"gobackupper/internal/process"
)

type Client struct {
	Runner process.Runner
	Binary string
}

func New(r process.Runner) *Client { return &Client{Runner: r} }
func (c *Client) binary() string {
	if c.Binary != "" {
		return c.Binary
	}
	return "rsync"
}

var versionRE = regexp.MustCompile(`(?m)^rsync  version ([0-9]+)\.([0-9]+)`)

func (c *Client) Check(ctx context.Context) error {
	name := c.binary()
	if _, e := exec.LookPath(name); e != nil {
		return fmt.Errorf("rsync dependency %q not found: %w", name, e)
	}
	res, e := c.Runner.Run(ctx, name, []string{"--version"}, nil)
	if e != nil {
		return fmt.Errorf("check rsync version: %w", e)
	}
	m := versionRE.FindStringSubmatch(res.Stdout)
	if m == nil {
		return fmt.Errorf("cannot determine rsync version from %q", res.Stdout)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < 3 || major == 3 && minor < 1 {
		return fmt.Errorf("rsync 3.1 or newer is required (found %s.%s)", m[1], m[2])
	}
	return nil
}

// Sync invokes system rsync without creating the destination. report is telemetry
// only and MUST return promptly; it is called synchronously while stdout is drained.
func (c *Client) Sync(ctx context.Context, job backup.Job, report func(backup.Progress)) (backup.SyncResult, error) {
	src, dst := job.SourcePath, job.Destination
	if !filepath.IsAbs(src) || !filepath.IsAbs(dst) {
		return backup.SyncResult{}, fmt.Errorf("source and destination must be absolute")
	}
	if !strings.HasSuffix(src, string(filepath.Separator)) {
		src += string(filepath.Separator)
	}
	if !strings.HasSuffix(dst, string(filepath.Separator)) {
		dst += string(filepath.Separator)
	}
	args := []string{"-ax"}
	if job.Delete {
		args = append(args, "--delete")
	}
	for _, x := range job.Excludes {
		args = append(args, "--exclude="+x)
	}
	args = append(args, "--info=progress2", "--outbuf=N", "--no-human-readable", "--out-format=GBU|%i|%l|%n", "--", src, dst)
	d := decoder{report: report}
	_, err := c.Runner.Run(ctx, c.binary(), args, &d)
	d.eof()
	final := backup.SyncResult{Progress: d.progress}
	if err != nil {
		return final, fmt.Errorf("rsync job %q: %w", job.Name, err)
	}
	return final, nil
}

type decoder struct {
	report   func(backup.Progress)
	progress backup.Progress
	pending  []byte
	dropping bool
}

func (d *decoder) Write(p []byte) (int, error) {
	n := len(p)
	for _, b := range p {
		if b == '\r' || b == '\n' {
			if !d.dropping {
				d.record(d.pending)
			}
			d.pending = d.pending[:0]
			d.dropping = false
			continue
		}
		if d.dropping {
			continue
		}
		if len(d.pending) >= 64<<10 {
			d.dropping = true
			d.progress.Degraded = true
			continue
		}
		d.pending = append(d.pending, b)
	}
	return n, nil
}
func (d *decoder) eof() {
	if len(d.pending) > 0 && !d.dropping {
		d.record(d.pending)
	}
	d.pending = nil
}
func (d *decoder) record(b []byte) {
	s := string(b)
	if strings.HasPrefix(s, "GBU|") {
		f := strings.SplitN(s, "|", 4)
		if len(f) == 4 {
			if _, err := strconv.ParseUint(f[2], 10, 64); err == nil && len(f[1]) > 0 && f[1][0] == '>' {
				d.progress.Current = decodeOctal(f[3])
				d.emit()
			}
			return
		}
	}
	p := parseProgress(s)
	if p != nil {
		d.progress.Bytes = p.Bytes
		d.progress.HasProgress = true
		d.progress.Rate = p.Rate
		d.progress.Percent = p.Percent
		if p.HasTransferred {
			d.progress.Transferred = p.Transferred
		}
		if p.HasChecks {
			d.progress.Remaining = p.Remaining
			d.progress.Total = p.Total
			d.progress.TotalKnown = p.TotalKnown
		}
		d.emit()
	}
}
func (d *decoder) emit() {
	if d.report != nil {
		d.report(d.progress)
	}
}

type parsed struct {
	Bytes                                 uint64
	Percent                               int
	Rate                                  string
	Transferred, Remaining, Total         uint64
	TotalKnown, HasTransferred, HasChecks bool
}

func parseProgress(s string) *parsed {
	f := strings.Fields(s)
	if len(f) < 3 {
		return nil
	}
	b, e := strconv.ParseUint(strings.ReplaceAll(f[0], ",", ""), 10, 64)
	if e != nil || !strings.HasSuffix(f[1], "%") {
		return nil
	}
	pc, e := strconv.Atoi(strings.TrimSuffix(f[1], "%"))
	if e != nil || pc < 0 || pc > 100 {
		return nil
	}
	p := &parsed{Bytes: b, Percent: pc, Rate: f[2]}
	for _, x := range f[3:] {
		x = strings.Trim(x, "(),")
		if strings.HasPrefix(x, "xfr#") {
			if n, e := strconv.ParseUint(strings.TrimPrefix(x, "xfr#"), 10, 64); e == nil {
				p.Transferred = n
				p.HasTransferred = true
			}
		}
		for _, prefix := range []string{"to-chk=", "ir-chk="} {
			if strings.HasPrefix(x, prefix) {
				a := strings.SplitN(strings.TrimPrefix(x, prefix), "/", 2)
				if len(a) == 2 {
					remaining, e1 := strconv.ParseUint(a[0], 10, 64)
					total, e2 := strconv.ParseUint(a[1], 10, 64)
					if e1 == nil && e2 == nil {
						p.Remaining = remaining
						p.Total = total
						p.HasChecks = true
						p.TotalKnown = prefix == "to-chk="
					}
				}
			}
		}
	}
	return p
}
func decodeOctal(s string) string {
	b := []byte(s)
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		if i+4 < len(b) && b[i] == '\\' && b[i+1] == '#' && b[i+2] >= '0' && b[i+2] <= '7' && b[i+3] >= '0' && b[i+3] <= '7' && b[i+4] >= '0' && b[i+4] <= '7' {
			out = append(out, (b[i+2]-'0')*64+(b[i+3]-'0')*8+b[i+4]-'0')
			i += 5
		} else {
			out = append(out, b[i])
			i++
		}
	}
	return string(out)
}
