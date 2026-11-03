package backup

import (
	"context"
	"errors"
	"time"
)

// Service coordinates policy. Progress callbacks must only update cheap latest-state data.
type Service struct {
	Sync       Synchronizer
	Repository Repository
	Now        func() time.Time
	OnEvent    func(Event)
	OnProgress func(Job, Progress)
}

func (s *Service) event(stage Stage, job Job, snapshot Snapshot) {
	if s.OnEvent != nil {
		s.OnEvent(Event{Stage: stage, Job: job, Snapshot: snapshot})
	}
}

// Run stops on the first failure and retains successful preceding job results.
func (s *Service) Run(ctx context.Context, plan Plan) (result RunResult, err error) {
	prepareError := func(e error) error { return &Error{Stage: StagePrepare, Err: e} }
	if e := ctx.Err(); e != nil {
		return result, prepareError(e)
	}
	if e := Validate(plan); e != nil {
		return result, prepareError(e)
	}
	if e := s.Sync.Check(ctx); e != nil {
		return result, prepareError(e)
	}
	if e := s.Repository.Check(ctx); e != nil {
		return result, prepareError(e)
	}
	release, e := s.Repository.Acquire(ctx)
	if e != nil {
		return result, prepareError(e)
	}
	defer func() {
		if e := release(); e != nil {
			err = errors.Join(err, prepareError(e))
		}
	}()
	if e := Validate(plan); e != nil {
		return result, prepareError(e)
	}
	workspaces := make([]Workspace, 0, len(plan.Jobs))
	for _, job := range plan.Jobs {
		if e := ctx.Err(); e != nil {
			return result, &Error{Stage: StagePrepare, Job: job.Name, Err: e}
		}
		s.event(StagePrepare, job, Snapshot{})
		workspace, e := s.Repository.Prepare(ctx, job)
		if e != nil {
			return result, &Error{Stage: StagePrepare, Job: job.Name, Err: e}
		}
		workspaces = append(workspaces, workspace)
	}
	now := s.Now
	if now == nil {
		now = time.Now
	}
	for _, workspace := range workspaces {
		job := workspace.Job
		if e := ctx.Err(); e != nil {
			return result, &Error{Stage: StageSync, Job: job.Name, Err: e}
		}
		s.event(StageSync, job, Snapshot{})
		_, e := s.Sync.Sync(ctx, job, func(p Progress) {
			if s.OnProgress != nil {
				s.OnProgress(job, p)
			}
		})
		if e != nil {
			return result, &Error{Stage: StageSync, Job: job.Name, Err: e}
		}
		if e := ctx.Err(); e != nil {
			return result, &Error{Stage: StageSync, Job: job.Name, Err: e}
		}
		date := now().Local()
		s.event(StageSnapshot, job, Snapshot{})
		snapshot, e := s.Repository.Snapshot(ctx, workspace, date)
		if e != nil {
			return result, &Error{Stage: StageSnapshot, Job: job.Name, Err: e}
		}
		result.Jobs = append(result.Jobs, JobResult{Job: job, Snapshot: snapshot})
		s.event(StageSuccess, job, snapshot)
	}
	return result, nil
}
