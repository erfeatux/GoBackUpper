package rsync

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gobackupper/internal/backup"
	"gobackupper/internal/process"
)

func TestDecoderChunkFramingOverflowAndProgressSuffixes(t *testing.T) {
	var got []backup.Progress
	d := decoder{report: func(p backup.Progress) { got = append(got, p) }}
	chunks := []string{"  123 50% 1.2MB/s 0:00:01 (xfr#2, ir-chk=4/9)\r\n", strings.Repeat("x", (64<<10)+1) + "\n", "  456 100% 2.0MB/s 0:00:02 (xfr#3, to-chk=0/9)"}
	for _, s := range chunks {
		for i := 0; i < len(s); i += 7 {
			end := i + 7
			if end > len(s) {
				end = len(s)
			}
			_, _ = d.Write([]byte(s[i:end]))
		}
	}
	d.eof()
	if len(got) != 2 {
		t.Fatalf("got %d progress updates", len(got))
	}
	if got[0].Bytes != 123 || got[0].TotalKnown || got[0].Remaining != 4 {
		t.Fatalf("bad incremental sample: %+v", got[0])
	}
	if got[1].Bytes != 456 || !got[1].TotalKnown || got[1].Total != 9 {
		t.Fatalf("bad final sample: %+v", got[1])
	}
	if !d.progress.Degraded {
		t.Fatal("overflow did not degrade telemetry")
	}
}
func TestDecoderEscapesFileNameOnceAndSafely(t *testing.T) {
	d := decoder{}
	d.record([]byte("GBU|>f+++++++++|3|a|\\#012\\#134#012\x1b"))
	want := "a|\n\\#012\x1b"
	if d.progress.Current != want {
		t.Fatalf("filename %q, want %q", d.progress.Current, want)
	}
	d.record([]byte("GBU|*deleting |3|old"))
	if d.progress.Current != want {
		t.Fatalf("metadata event replaced current filename: %q", d.progress.Current)
	}
}
func TestRsyncCopiesDeleteExcludeAndSymlink(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync not installed")
	}
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	if err := os.Mkdir(src, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dst, 0700); err != nil {
		t.Fatal(err)
	}
	put := func(path, data string) {
		t.Helper()
		if e := os.WriteFile(filepath.Join(path, data), []byte(data), 0600); e != nil {
			t.Fatal(e)
		}
	}
	put(src, "keep.txt")
	put(src, "skip.tmp")
	put(dst, "stale.txt")
	if err := os.Symlink("keep.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	c := New(process.Runner{})
	_, err := c.Sync(context.Background(), backup.Job{Name: "test", SourcePath: src, Destination: dst, Delete: true, Excludes: []string{"*.tmp"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, e := os.ReadFile(filepath.Join(dst, "keep.txt")); e != nil || string(b) != "keep.txt" {
		t.Fatalf("copied content %q, %v", b, e)
	}
	if _, e := os.Lstat(filepath.Join(dst, "stale.txt")); !os.IsNotExist(e) {
		t.Fatalf("stale destination remains: %v", e)
	}
	if _, e := os.Lstat(filepath.Join(dst, "skip.tmp")); !os.IsNotExist(e) {
		t.Fatalf("excluded file copied: %v", e)
	}
	info, e := os.Lstat(filepath.Join(dst, "link"))
	if e != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink not preserved: %v, %v", info, e)
	}
}
func TestRsyncFailureIsReturned(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync not installed")
	}
	root := t.TempDir()
	dst := filepath.Join(root, "dst")
	if err := os.Mkdir(dst, 0700); err != nil {
		t.Fatal(err)
	}
	_, err := New(process.Runner{}).Sync(context.Background(), backup.Job{Name: "missing", SourcePath: filepath.Join(root, "absent"), Destination: dst}, nil)
	if err == nil {
		t.Fatal("missing source unexpectedly succeeded")
	}
}
