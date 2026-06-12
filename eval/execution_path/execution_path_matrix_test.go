package execution_path

import (
	"testing"

	"github.com/compshare-agent/internal/observability"
)

func TestActualExecutionPathMatrix(t *testing.T) {
	cases := []struct {
		name   string
		record observability.TraceRecord
		want   string
	}{
		{
			name: "deterministic routing dispatch",
			record: observability.TraceRecord{
				IntentRouter: observability.RouterTrace{RouteStatus: "dispatched"},
			},
			want: observability.ExecutionPathRouting,
		},
		{
			name: "deterministic routing clarification",
			record: observability.TraceRecord{
				IntentRouter: observability.RouterTrace{RouteStatus: "selection_required"},
			},
			want: observability.ExecutionPathRouting,
		},
		{
			name: "terminal cited retrieval",
			record: observability.TraceRecord{
				IntentRouter: observability.RouterTrace{RouteStatus: "dispatched_retrieval"},
			},
			want: observability.ExecutionPathTerminalRAG,
		},
		{
			name: "retrieval evidence inside diagnosis remains agent",
			record: observability.TraceRecord{
				Retrieval: observability.RetrievalTrace{Enabled: true, Hits: 2},
				ToolCalls: []observability.ToolCallTrace{{
					Source: observability.ToolSourceDiagnosisInternal,
				}},
			},
			want: observability.ExecutionPathAgent,
		},
		{
			name: "saga workflow remains agent",
			record: observability.TraceRecord{
				Steps: []observability.StepTrace{{
					StepID: "create-image-confirm",
					State:  observability.StepStateAwaitingConfirm,
				}},
			},
			want: observability.ExecutionPathAgent,
		},
		{
			name: "main tool loop remains agent",
			record: observability.TraceRecord{
				ToolCalls: []observability.ToolCallTrace{{
					Source: observability.ToolSourceMainReAct,
				}},
			},
			want: observability.ExecutionPathAgent,
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
			if got := tc.record.DeriveActualExecutionPath(); got != tc.want {
				t.Fatalf("DeriveActualExecutionPath() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPlannedActualExecutionPathMismatchMatrix(t *testing.T) {
	cases := []struct {
		name         string
		record       observability.TraceRecord
		wantMismatch bool
		wantCounted  bool
	}{
		{
			name: "routing planned and routing executed",
			record: observability.TraceRecord{
				IntentRouter:           observability.RouterTrace{PlannedExecutionPath: observability.ExecutionPathRouting},
				ActualExecutionPath: observability.ExecutionPathRouting,
			},
			wantMismatch: false,
			wantCounted:  true,
		},
		{
			name: "terminal rag planned and terminal rag executed",
			record: observability.TraceRecord{
				IntentRouter:           observability.RouterTrace{PlannedExecutionPath: observability.ExecutionPathTerminalRAG},
				ActualExecutionPath: observability.ExecutionPathTerminalRAG,
			},
			wantMismatch: false,
			wantCounted:  true,
		},
		{
			name: "agent planned and saga executed",
			record: observability.TraceRecord{
				IntentRouter:           observability.RouterTrace{PlannedExecutionPath: observability.ExecutionPathAgent},
				ActualExecutionPath: observability.ExecutionPathAgent,
			},
			wantMismatch: false,
			wantCounted:  true,
		},
		{
			name: "tutorial planned but diagnosis actually ran",
			record: observability.TraceRecord{
				IntentRouter:           observability.RouterTrace{PlannedExecutionPath: observability.ExecutionPathTerminalRAG},
				ActualExecutionPath: observability.ExecutionPathAgent,
			},
			wantMismatch: true,
			wantCounted:  true,
		},
		{
			name: "hard block with no actual form is excluded",
			record: observability.TraceRecord{
				IntentRouter:         observability.RouterTrace{PlannedExecutionPath: observability.ExecutionPathAgent},
				EngineHardBlock: observability.EngineHardBlockTrace{Hit: true, Category: "account_billing"},
			},
			wantMismatch: false,
			wantCounted:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotMismatch, gotCounted := tc.record.ExecutionPathMismatch()
			if gotMismatch != tc.wantMismatch || gotCounted != tc.wantCounted {
				t.Fatalf("ExecutionPathMismatch() = (%v, %v), want (%v, %v)", gotMismatch, gotCounted, tc.wantMismatch, tc.wantCounted)
			}
		})
	}
}
