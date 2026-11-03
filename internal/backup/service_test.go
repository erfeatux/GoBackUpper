package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type memorySync struct {
	run func(context.Context, Job) error
}

func (m memorySync) Check(context.Context) error { return nil }
func (m memorySync) Sync(ctx context.Context, j Job, _ func(Progress)) (SyncResult, error) {
	return SyncResult{}, m.run(ctx, j)
}

type recordingRepository struct {
	root         string
	failPrepare  string
	failSnapshot string
	released     bool
	dates        []time.Time
}

func (r *recordingRepository) Check(context.Context) error { return nil }
func (r *recordingRepository) Acquire(context.Context) (func() error, error) {
	return func() error { r.released = true; return nil }, nil
}
func (r *recordingRepository) Prepare(_ context.Context, j Job) (Workspace, error) {
	if j.Name == r.failPrepare {
		return Workspace{}, errors.New("prepare denied")
	}
	return Workspace{Job: j}, nil
}
func (r *recordingRepository) Snapshot(_ context.Context, w Workspace, date time.Time) (Snapshot, error) {
	if w.Job.Name == r.failSnapshot {
		return Snapshot{}, errors.New("snapshot denied")
	}
	r.dates = append(r.dates, date)
	path := filepath.Join(r.root, w.Job.Name+"-"+date.Format("2006.01.02"))
	if err := os.WriteFile(path, []byte("saved "+w.Job.Name), 0600); err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Path: path}, nil
}

func TestServiceSyncFailureKeepsPreviousSnapshotAndStopsFollowingJobs(t *testing.T) {
	plan := testPlan(t)
	root := t.TempDir()
	previous := filepath.Join(root, "previous")
	if err := os.WriteFile(previous, []byte("old data"), 0600); err != nil {
		t.Fatal(err)
	}
	repo := &recordingRepository{root: root}
	failure := errors.New("rsync exit 23")
	service := Service{Repository: repo, Sync: memorySync{run: func(_ context.Context, j Job) error {
		if j.Name != "A" {
			t.Fatal("next job executed after failed sync")
		}
		return failure
	}}}
	result, err := service.Run(context.Background(), plan)
	var stage *Error
	if !errors.Is(err, failure) || !errors.As(err, &stage) || stage.Stage != StageSync {
		t.Fatalf("error: %v", err)
	}
	if len(result.Jobs) != 0 || !repo.released {
		t.Fatalf("result/release: %+v %t", result, repo.released)
	}
	entries, e := os.ReadDir(root)
	if e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(previous)
	if e != nil || string(data) != "old data" || len(entries) != 1 {
		t.Fatalf("old snapshot changed: %s %v", data, e)
	}
}

func TestServicePreparationFailureCopiesNothing(t *testing.T) {
	plan := testPlan(t)
	repo := &recordingRepository{root: t.TempDir(), failPrepare: "B"}
	service := Service{Repository: repo, Sync: memorySync{run: func(context.Context, Job) error { t.Fatal("copy before all preparation succeeded"); return nil }}}
	_, err := service.Run(context.Background(), plan)
	var stage *Error
	if !errors.As(err, &stage) || stage.Stage != StagePrepare || stage.Job != "B" || !repo.released {
		t.Fatalf("error: %v", err)
	}
}

func TestServiceDatesAfterEachSyncAndKeepsPartialSuccess(t *testing.T) {
	plan := testPlan(t)
	repo := &recordingRepository{root: t.TempDir(), failSnapshot: "B"}
	local := time.Local
	clock := time.Date(2026, 11, 3, 23, 59, 59, 0, local)
	service := Service{Repository: repo, Now: func() time.Time { return clock }, Sync: memorySync{run: func(_ context.Context, j Job) error {
		if j.Name == "A" {
			clock = clock.Add(2 * time.Second)
		}
		return nil
	}}}
	result, err := service.Run(context.Background(), plan)
	var stage *Error
	if !errors.As(err, &stage) || stage.Stage != StageSnapshot || stage.Job != "B" {
		t.Fatalf("error: %v", err)
	}
	if len(result.Jobs) != 1 || result.Jobs[0].Job.Name != "A" {
		t.Fatalf("successful results lost: %+v", result)
	}
	data, e := os.ReadFile(result.Jobs[0].Snapshot.Path)
	if e != nil || string(data) != "saved A" {
		t.Fatalf("successful snapshot lost: %q %v", data, e)
	}
	if repo.dates[0].Format("2006.01.02") != "2026.11.04" {
		t.Fatalf("date determined before sync: %v", repo.dates)
	}
	if !repo.released {
		t.Fatal("lock not released")
	}
}

func TestServiceCancellationAfterSyncDoesNotSnapshot(t *testing.T) {
	plan := testPlan(t)
	repo := &recordingRepository{root: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := Service{Repository: repo, Sync: memorySync{run: func(context.Context, Job) error { cancel(); return nil }}}
	_, err := service.Run(ctx, plan)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error: %v", err)
	}
	entries, e := os.ReadDir(repo.root)
	if e != nil {
		t.Fatal(e)
	}
	if len(entries) != 0 || !repo.released {
		t.Fatal("snapshot after cancellation or lock retained")
	}
}
