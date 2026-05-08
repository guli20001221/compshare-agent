package intent

import (
	"context"
	"time"

	"github.com/compshare-agent/internal/observability"
)

type ShadowPlanner interface {
	Plan(ctx context.Context, input PlannerInput) (PlannerResult, error)
}

type ShadowRunnerOptions struct {
	Enabled bool
	Model   string
	Now     func() time.Time
}

type ShadowRunner struct {
	planner ShadowPlanner
	enabled bool
	model   string
	now     func() time.Time
}

func NewShadowRunner(planner ShadowPlanner, opts ShadowRunnerOptions) *ShadowRunner {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &ShadowRunner{
		planner: planner,
		enabled: opts.Enabled,
		model:   opts.Model,
		now:     now,
	}
}

func (r *ShadowRunner) Run(ctx context.Context, input PlannerInput) observability.PlannerTrace {
	if r == nil || !r.enabled {
		return ProjectPlannerTrace(PlannerResult{}, PlannerTraceOptions{Enabled: false})
	}

	start := r.now()
	result := PlannerResult{
		Fallback: true,
		Plan:     unknownFallbackPlan(),
	}
	if r.planner != nil {
		planned, err := r.planner.Plan(ctx, input)
		if err == nil {
			result = planned
		}
	}
	latency := r.now().Sub(start)
	return ProjectPlannerTrace(result, PlannerTraceOptions{
		Enabled: true,
		Model:   r.model,
		Latency: latency,
	})
}
