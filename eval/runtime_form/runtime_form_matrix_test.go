package runtime_form

import (
	"testing"

	"github.com/compshare-agent/internal/observability"
)

func TestActualRuntimeFormMatrix(t *testing.T) {
	cases := []struct {
		name   string
		record observability.TraceRecord
		want   string
	}{
		{
			name: "deterministic routing dispatch",
			record: observability.TraceRecord{
				Planner: observability.RouterTrace{RouteStatus: "dispatched"},
			},
			want: observability.RuntimeFormRouting,
		},
		{
			name: "deterministic routing clarification",
			record: observability.TraceRecord{
				Planner: observability.RouterTrace{RouteStatus: "selection_required"},
			},
			want: observability.RuntimeFormRouting,
		},
		{
			name: "terminal cited retrieval",
			record: observability.TraceRecord{
				Planner: observability.RouterTrace{RouteStatus: "dispatched_retrieval"},
			},
			want: observability.RuntimeFormTerminalRAG,
		},
		{
			name: "retrieval evidence inside diagnosis remains agent",
			record: observability.TraceRecord{
				Retrieval: observability.RetrievalTrace{Enabled: true, Hits: 2},
				ToolCalls: []observability.ToolCallTrace{{
					Source: observability.ToolSourceDiagnosisInternal,
				}},
			},
			want: observability.RuntimeFormAgent,
		},
		{
			name: "saga workflow remains agent",
			record: observability.TraceRecord{
				Steps: []observability.StepTrace{{
					StepID: "create-image-confirm",
					State:  observability.StepStateAwaitingConfirm,
				}},
			},
			want: observability.RuntimeFormAgent,
		},
		{
			name: "main tool loop remains agent",
			record: observability.TraceRecord{
				ToolCalls: []observability.ToolCallTrace{{
					Source: observability.ToolSourceMainReAct,
				}},
			},
			want: observability.RuntimeFormAgent,
		},
		{
			name: "hard block or canned no-signal answer stays unknown",
			record: observability.TraceRecord{
				EngineHardBlock: observability.EngineHardBlockTrace{Hit: true, Category: "account_billing"},
			},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.record.DeriveActualRuntimeForm(); got != tc.want {
				t.Fatalf("DeriveActualRuntimeForm() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPlannedActualRuntimeFormMismatchMatrix(t *testing.T) {
	cases := []struct {
		name         string
		record       observability.TraceRecord
		wantMismatch bool
		wantCounted  bool
	}{
		{
			name: "routing planned and routing executed",
			record: observability.TraceRecord{
				Planner:           observability.RouterTrace{PlannedRuntimeForm: observability.RuntimeFormRouting},
				ActualRuntimeForm: observability.RuntimeFormRouting,
			},
			wantMismatch: false,
			wantCounted:  true,
		},
		{
			name: "terminal rag planned and terminal rag executed",
			record: observability.TraceRecord{
				Planner:           observability.RouterTrace{PlannedRuntimeForm: observability.RuntimeFormTerminalRAG},
				ActualRuntimeForm: observability.RuntimeFormTerminalRAG,
			},
			wantMismatch: false,
			wantCounted:  true,
		},
		{
			name: "agent planned and saga executed",
			record: observability.TraceRecord{
				Planner:           observability.RouterTrace{PlannedRuntimeForm: observability.RuntimeFormAgent},
				ActualRuntimeForm: observability.RuntimeFormAgent,
			},
			wantMismatch: false,
			wantCounted:  true,
		},
		{
			name: "tutorial planned but diagnosis actually ran",
			record: observability.TraceRecord{
				Planner:           observability.RouterTrace{PlannedRuntimeForm: observability.RuntimeFormTerminalRAG},
				ActualRuntimeForm: observability.RuntimeFormAgent,
			},
			wantMismatch: true,
			wantCounted:  true,
		},
		{
			name: "hard block with no actual form is excluded",
			record: observability.TraceRecord{
				Planner:         observability.RouterTrace{PlannedRuntimeForm: observability.RuntimeFormAgent},
				EngineHardBlock: observability.EngineHardBlockTrace{Hit: true, Category: "account_billing"},
			},
			wantMismatch: false,
			wantCounted:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotMismatch, gotCounted := tc.record.RuntimeFormMismatch()
			if gotMismatch != tc.wantMismatch || gotCounted != tc.wantCounted {
				t.Fatalf("RuntimeFormMismatch() = (%v, %v), want (%v, %v)", gotMismatch, gotCounted, tc.wantMismatch, tc.wantCounted)
			}
		})
	}
}
