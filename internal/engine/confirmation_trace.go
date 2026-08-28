package engine

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/compshare-agent/internal/guardrails"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/workflow"
)

// recordConfirmationResult is the single bridge from the confirmation harness
// into the trace. It is called after the transport has stopped waiting, so the
// elapsed time measures user-facing card latency rather than model time.
func (e *Engine) recordConfirmationResult(action string, result ConfirmationResult, started time.Time, args map[string]any, form *workflow.ConfirmForm) {
	if e == nil {
		return
	}
	reason := observability.NormalizeConfirmationTerminalReason(result.Confirmed, result.TerminalReason)
	if e.confirmationTraceObserver != nil {
		trace := observability.ConfirmationTrace{
			Action:         action,
			State:          observability.ConfirmationStateForTerminalReason(reason),
			TerminalReason: reason,
			ElapsedMS:      time.Since(started).Milliseconds(),
		}
		if form != nil && form.Step != nil {
			stepIndex := form.Step.Index
			final := form.Step.Final
			trace.StepIndex = &stepIndex
			trace.StepTitle = strings.TrimSpace(form.Step.Title)
			trace.Final = &final
			if result.Confirmed && form.Step.Final && action == "CreateInstanceWorkflow" {
				trace.ConfirmedContract = confirmedCreateContractFromArgs(args)
			}
		}
		e.confirmationTraceObserver(trace)
	}
}

func confirmedCreateContractFromArgs(args map[string]any) *observability.ConfirmedCreateContract {
	if len(args) == 0 {
		return nil
	}
	contract := &observability.ConfirmedCreateContract{
		GPUType:        confirmationArgText(args, "GpuType"),
		GPU:            confirmationArgInt(args, "Gpu"),
		CPU:            confirmationArgInt(args, "CPU"),
		MemoryMB:       confirmationArgInt(args, "Memory"),
		Zone:           confirmationArgText(args, "Zone"),
		ZoneLabel:      confirmationArgText(args, "ZoneLabel"),
		Image:          confirmationArgText(args, "image"),
		SystemDisk:     confirmationArgText(args, "SystemDisk"),
		DataDisk:       confirmationArgText(args, "DataDisk"),
		ChargeType:     confirmationArgText(args, "ChargeType"),
		EstimatedPrice: confirmationArgText(args, "price"),
	}
	if *contract == (observability.ConfirmedCreateContract{}) {
		return nil
	}
	return contract
}

func confirmationArgText(args map[string]any, key string) string {
	value, ok := args[key]
	if !ok || value == nil {
		return ""
	}
	text := strings.TrimSpace(fmt.Sprint(value))
	return guardrails.RedactOutputLeak(guardrails.RedactPII(text))
}

func confirmationArgInt(args map[string]any, key string) int {
	value := confirmationArgText(args, key)
	if value == "" {
		return 0
	}
	n, _ := strconv.Atoi(value)
	return n
}
