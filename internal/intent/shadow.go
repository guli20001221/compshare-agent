package intent

import (
	"context"
	"time"

	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/observability"
)

type ShadowPlanner interface {
	Plan(ctx context.Context, input PlannerInput) (PlannerResult, error)
}

type ShadowRunnerOptions struct {
	Enabled bool
	Model   string
	// BaseURL is intentionally omitted because trace.v0.1 PlannerTrace has no
	// BaseURL field. Add it only if a future trace schema includes it.
	Now func() time.Time
	// QuotaSubject must be a non-secret subject key, such as the hashed value
	// from governance.SubjectKeyFromPublicKey. ShadowRunner never writes it to
	// PlannerTrace.
	QuotaSubject string
	QuotaHook    func(governance.Request) governance.Decision
}

type ShadowRunner struct {
	planner      ShadowPlanner
	enabled      bool
	model        string
	now          func() time.Time
	quotaSubject string
	quotaHook    func(governance.Request) governance.Decision
}

func NewShadowRunner(planner ShadowPlanner, opts ShadowRunnerOptions) *ShadowRunner {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &ShadowRunner{
		planner:      planner,
		enabled:      opts.Enabled,
		model:        opts.Model,
		now:          now,
		quotaSubject: opts.QuotaSubject,
		quotaHook:    opts.QuotaHook,
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
	if r.quotaHook != nil {
		decision := r.quotaHook(governance.Request{
			SubjectKey: r.quotaSubject,
			Class:      governance.ClassLLM,
			Action:     "shadow_planner",
		})
		if !decision.Allowed {
			return ProjectPlannerTrace(result, PlannerTraceOptions{
				Enabled: true,
				Model:   r.model,
				Latency: r.now().Sub(start),
			})
		}
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
