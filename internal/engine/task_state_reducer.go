package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/tools"
)

type TaskRelation string

const (
	TaskRelationContinue TaskRelation = "continue"
	TaskRelationReplace  TaskRelation = "replace"
	TaskRelationComplete TaskRelation = "complete"
	TaskRelationClear    TaskRelation = "clear"
)

type TaskStateDelta struct {
	Relation TaskRelation   `json:"relation"`
	Task     *TaskDeltaBody `json:"task,omitempty"`
}

type TaskDeltaBody struct {
	Goal         string    `json:"goal,omitempty"`
	Intent       string    `json:"intent,omitempty"`
	Workflow     string    `json:"workflow,omitempty"`
	Stage        string    `json:"stage,omitempty"`
	Constraints  *[]string `json:"constraints,omitempty"`
	Decisions    *[]string `json:"decisions,omitempty"`
	MissingSlots *[]string `json:"missing_slots,omitempty"`
}

type TaskStateResult struct {
	Applied  bool          `json:"applied"`
	Relation TaskRelation  `json:"relation"`
	Task     *TaskSnapshot `json:"task,omitempty"`
	Error    string        `json:"error,omitempty"`
}

func decodeTaskStateDelta(args map[string]any) (TaskStateDelta, error) {
	payload, err := json.Marshal(args)
	if err != nil {
		return TaskStateDelta{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var delta TaskStateDelta
	if err := decoder.Decode(&delta); err != nil {
		return TaskStateDelta{}, err
	}
	switch delta.Relation {
	case TaskRelationContinue, TaskRelationReplace, TaskRelationComplete, TaskRelationClear:
	default:
		return TaskStateDelta{}, fmt.Errorf("unknown task relation")
	}
	return delta, nil
}

// reduceTaskState is the only P6 task-state mutation. It consumes structured
// Agent output and never reads the user's sentence. Task state is understanding
// only and cannot authorize a workflow target or action.
func reduceTaskState(current TaskSnapshot, delta TaskStateDelta, now time.Time) (TaskSnapshot, error) {
	sanitizeItems := func(items *[]string) *[]string {
		if items == nil {
			return nil
		}
		clean := safeContextItems(*items)
		return &clean
	}
	sanitize := func(body *TaskDeltaBody) TaskDeltaBody {
		if body == nil {
			return TaskDeltaBody{}
		}
		return TaskDeltaBody{
			Goal: safeContextText(body.Goal), Intent: safeContextText(body.Intent), Workflow: safeContextText(body.Workflow), Stage: safeContextText(body.Stage),
			Constraints: sanitizeItems(body.Constraints), Decisions: sanitizeItems(body.Decisions), MissingSlots: sanitizeItems(body.MissingSlots),
		}
	}
	body := sanitize(delta.Task)
	switch delta.Relation {
	case TaskRelationReplace:
		if strings.TrimSpace(body.Goal) == "" {
			return current, fmt.Errorf("replacement task requires a goal")
		}
		return TaskSnapshot{Goal: body.Goal, Intent: body.Intent, Workflow: body.Workflow, Stage: body.Stage,
			Constraints: valueOrEmpty(body.Constraints), Decisions: valueOrEmpty(body.Decisions), MissingSlots: valueOrEmpty(body.MissingSlots),
			Status: TaskSnapshotStatusActive, Freshness: ContinuityFreshnessFresh, UpdatedAtUnix: now.Unix()}, nil
	case TaskRelationContinue:
		if taskSnapshotEmpty(current) || current.Status == TaskSnapshotStatusResolved {
			return current, fmt.Errorf("there is no active task to continue")
		}
		if body.Intent != "" && current.Intent != "" && body.Intent != current.Intent {
			return current, fmt.Errorf("task intent conflicts with the active task")
		}
		if body.Workflow != "" && current.Workflow != "" && body.Workflow != current.Workflow {
			return current, fmt.Errorf("task workflow conflicts with the active task")
		}
		if body.Goal != "" {
			current.Goal = body.Goal
		}
		if body.Intent != "" {
			current.Intent = body.Intent
		}
		if body.Workflow != "" {
			current.Workflow = body.Workflow
		}
		if body.Stage != "" {
			current.Stage = body.Stage
		}
		if body.Constraints != nil {
			current.Constraints = *body.Constraints
		}
		if body.Decisions != nil {
			current.Decisions = *body.Decisions
		}
		if body.MissingSlots != nil {
			current.MissingSlots = *body.MissingSlots
		}
		current.Status, current.Freshness, current.EndReason, current.UpdatedAtUnix = TaskSnapshotStatusActive, ContinuityFreshnessFresh, "", now.Unix()
		return current, nil
	case TaskRelationComplete:
		if taskSnapshotEmpty(current) {
			return current, fmt.Errorf("there is no task to complete")
		}
		current.Status, current.EndReason, current.UpdatedAtUnix = TaskSnapshotStatusResolved, "completed_by_agent_delta", now.Unix()
		return current, nil
	case TaskRelationClear:
		return TaskSnapshot{}, nil
	default:
		return current, fmt.Errorf("unknown task relation")
	}
}

func valueOrEmpty(items *[]string) []string {
	if items == nil {
		return nil
	}
	return *items
}

func (e *Engine) executeTaskStateDelta(args map[string]any, onStep func(StepEvent)) string {
	if !e.centralAgentRuntimeEnabled {
		payload, _ := json.Marshal(TaskStateResult{Error: "central Agent runtime is not enabled"})
		return string(payload)
	}
	delta, err := decodeTaskStateDelta(args)
	if err != nil {
		return taskStateError(delta.Relation, err, onStep)
	}
	updated, err := reduceTaskState(e.sessionState.TaskSnapshot, delta, time.Now())
	if err != nil {
		return taskStateError(delta.Relation, err, onStep)
	}
	e.sessionState.TaskSnapshot = updated
	result := TaskStateResult{Applied: true, Relation: delta.Relation}
	if !taskSnapshotEmpty(updated) {
		copy := updated
		result.Task = &copy
	}
	payload, _ := json.Marshal(result)
	var trace map[string]any
	_ = json.Unmarshal(payload, &trace)
	onStep(StepEvent{Type: StepToolResult, Action: tools.UpdateTaskStateName, Source: observability.ToolSourceMainReAct, Message: "任务状态已归并", TraceResult: trace})
	return string(payload)
}

func taskStateError(relation TaskRelation, err error, onStep func(StepEvent)) string {
	onStep(StepEvent{Type: StepError, Action: tools.UpdateTaskStateName, Source: observability.ToolSourceMainReAct, Message: err.Error()})
	payload, _ := json.Marshal(TaskStateResult{Relation: relation, Error: err.Error()})
	return string(payload)
}
