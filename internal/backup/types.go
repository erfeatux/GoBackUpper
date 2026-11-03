// Package backup defines backup policy independently of command-line and storage adapters.
package backup

import (
	"context"
	"fmt"
	"time"
)

// Origin identifies the user configuration that declared a job.
type Origin struct {
	File    string
	Section string
}

// Job is immutable after planning. Name is the source section name, not a path.
type Job struct {
	Name           string
	SourcePath     string
	Destination    string
	SnapshotPrefix string
	Delete         bool
	Excludes       []string
	Compression    string
	Origin         Origin
}

// Plan preserves declaration order and the configured storage layout.
type Plan struct {
	TargetRoot   string
	SnapshotRoot string
	Jobs         []Job
}

type Workspace struct{ Job Job }

type Snapshot struct {
	Path         string
	PreviousPath string
}

// Progress is best-effort telemetry, never evidence of backup success.
type Progress struct {
	Current     string
	Bytes       uint64
	Percent     int
	HasProgress bool
	Rate        string
	Transferred uint64
	Remaining   uint64
	Total       uint64
	TotalKnown  bool
	Degraded    bool
}

type SyncResult struct{ Progress Progress }

type Synchronizer interface {
	Check(context.Context) error
	Sync(context.Context, Job, func(Progress)) (SyncResult, error)
}

type Repository interface {
	Check(context.Context) error
	Acquire(context.Context) (func() error, error)
	Prepare(context.Context, Job) (Workspace, error)
	Snapshot(context.Context, Workspace, time.Time) (Snapshot, error)
}

type Stage string

const (
	StagePrepare  Stage = "prepare"
	StageSync     Stage = "sync"
	StageSnapshot Stage = "snapshot"
	StageSuccess  Stage = "success"
)

// Event contains reliable phase transitions. Progress uses a separate channel.
type Event struct {
	Stage    Stage
	Job      Job
	Snapshot Snapshot
}

type JobResult struct {
	Job      Job
	Snapshot Snapshot
}

type RunResult struct{ Jobs []JobResult }

// Error preserves the failed stage and cause for CLI exit mapping.
type Error struct {
	Stage Stage
	Job   string
	Err   error
}

func (e *Error) Error() string {
	if e.Job == "" {
		return fmt.Sprintf("%s: %v", e.Stage, e.Err)
	}
	return fmt.Sprintf("%s %q: %v", e.Stage, e.Job, e.Err)
}
func (e *Error) Unwrap() error { return e.Err }
