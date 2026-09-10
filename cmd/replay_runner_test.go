package main

// In-process replay runner shared by the live probes in this package.
//
// runCaseInProcess replicates the HTTP server's fresh-session setup
// (agentpool.buildEngine): NewSession + RehydrateHistory(nil), no Init(), and a
// per-turn ConfirmFunc that DECLINES (confirm=false) while recording the
// confirmation frame. A probe that hand-rolled this wiring would drift from the
// server's and stop measuring production.
//
// replayCaseRecord and writeReplayJSONL are the on-disk record shape those
// probes emit and that acceptance_judge_live_test.go reads back.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/compshare-agent/internal/engine"
)

type replayCaseRecord struct {
	CaseID     string    `json:"case_id"`
	Turns      []turnRec `json:"turns"`
	FinalReply string    `json:"final_reply,omitempty"`
	Error      string    `json:"error,omitempty"`
	// CitedChunkIDs is the chunk_ids the answer cited. Markers are stripped
	// before display, so the reply text can never show whether an answer was
	// evidence-backed; only the retrieval trace can. Filled by callers that
	// attach a retrieval observer (the live probe); empty otherwise.
	CitedChunkIDs   []string `json:"cited_chunk_ids,omitempty"`
	RetrievalTraces int      `json:"retrieval_traces,omitempty"`
	// RetrievedChunks is everything retrieval surfaced this turn, kept or
	// floor-dropped. Cited ids alone cannot tell a retrieval failure from a
	// synthesis failure: "the answer did not use chunk X" and "chunk X never
	// reached the agent" look identical without this.
	RetrievedChunks []retrievedChunkRec `json:"retrieved_chunks,omitempty"`
	// History is the prior conversation rehydrated before the live turn. It is
	// carried into the transcript because a follow-up cannot be judged without
	// it: "如何生成密钥？" is a complete question only against the exchange it
	// followed, and a judge reading the question alone would score the answer
	// for ambiguity the agent did not actually face.
	History []historyRec `json:"history,omitempty"`
}

type historyRec struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type retrievedChunkRec struct {
	ChunkID string  `json:"chunk_id"`
	Kept    bool    `json:"kept"`
	Score   float64 `json:"score"`
}

type turnRec struct {
	Index             int          `json:"index"`
	User              string       `json:"user"`
	Reply             string       `json:"reply,omitempty"`
	ErrorCode         string       `json:"error_code,omitempty"`
	ErrorMessage      string       `json:"error_message,omitempty"`
	Steps             []stepRec    `json:"steps,omitempty"`
	Confirmations     []confirmRec `json:"confirmations,omitempty"`
	ConfirmationCount int          `json:"confirmation_count"`
}

type stepRec struct {
	Type    string `json:"type,omitempty"`
	Action  string `json:"action,omitempty"`
	Message string `json:"message,omitempty"`
	// Args / Result are additive debug fields (the Python checker reads only
	// type+action). Without them a transcript proves only THAT a tool was
	// called, not that the reply's specifics came from it — which is the whole
	// question a live tool probe exists to answer. Result is StepEvent.TraceResult,
	// the already-redacted payload the trace layer uses.
	Args   map[string]any `json:"args,omitempty"`
	Result map[string]any `json:"result,omitempty"`
}

type confirmRec struct {
	Action string `json:"action,omitempty"`
}

// stepTypeWire mirrors httpapi.stepTypeString (the WS wire mapping the checker's
// recorded signal uses). Kept inline because that helper is unexported in
// internal/httpapi; the single source of truth is engine.StepType.
func stepTypeWire(t engine.StepType) string {
	switch t {
	case engine.StepToolCall:
		return "tool_call"
	case engine.StepToolResult:
		return "tool_result"
	case engine.StepConfirmNeeded:
		return "confirm_needed"
	case engine.StepBlocked:
		return "blocked"
	case engine.StepError:
		return "error"
	default:
		return "unknown"
	}
}

// runCaseInProcess replicates the HTTP server's fresh-session setup
// (agentpool.buildEngine): NewSession + RehydrateHistory(nil), no Init(), and a
// per-turn ConfirmFunc that DECLINES (confirm=false) while recording the
// confirmation frame. onStep records every StepEvent.
//
// base carries the caller's tenant identity: with legacy AK/SK empty the
// executor is STS-only, and tools.UserContext is what STSProvider.Get needs to
// AssumeRole. The gate passes context.Background() (no identity — CompShare
// tool calls surface an auth error, which the behavioral contract tolerates
// because it asserts WHICH tool was selected, not what it returned); the live
// tool probe passes a real tenant so the calls actually reach the API.
//
// subject is the rate-limit bucket. Cases replayed back-to-back under ONE
// subject trip agent.rate_limit.user_turn_qps (2/s per tenant) and answer
// "请求过于频繁", which looks like a model failure but is our own limiter; a
// caller replaying N distinct users' questions passes N distinct subjects.
// configure runs against the fresh session before its first turn — the seam for
// attaching trace observers (variadic so the gate's call site is unchanged).
//
// history is the prior conversation to rehydrate before the first live turn,
// exactly as the HTTP path loads a session's messages out of PostgreSQL. Pass
// nil for a fresh session. It exists for replaying a production transcript: the
// alternative — re-sending the user's earlier turns live and letting the agent
// generate its own replies — reconstructs a conversation that never happened,
// and the question under test is how the agent handles the follow-up given what
// the user ACTUALLY saw.
func runCaseInProcess(base context.Context, deps *engine.SharedDeps, mutating bool, subject, caseID string, history []engine.HistoryMessage, userTurns []string, timeout time.Duration, configure ...func(*engine.Engine)) *replayCaseRecord {
	eng := engine.NewSession(deps, engine.SessionOptions{
		Subject:              subject,
		ConfirmFn:            func(string, map[string]any) bool { return false },
		MutatingToolsEnabled: mutating,
	})
	for _, fn := range configure {
		if fn != nil {
			fn(eng)
		}
	}
	eng.RehydrateHistory(history) // nil = fresh session: system prompt + empty history

	rec := &replayCaseRecord{CaseID: caseID}
	for _, m := range history {
		rec.History = append(rec.History, historyRec{Role: m.Role, Content: m.Content})
	}
	for i, user := range userTurns {
		var steps []stepRec
		var confirms []confirmRec
		onStep := func(ev engine.StepEvent) {
			steps = append(steps, stepRec{
				Type:    stepTypeWire(ev.Type),
				Action:  ev.Action,
				Message: ev.Message,
				Args:    ev.Args,
				Result:  ev.TraceResult,
			})
		}
		confirmFn := func(action string, _ map[string]any) bool {
			confirms = append(confirms, confirmRec{Action: action})
			return false // decline — confirm=false replay mode
		}
		ctx, cancel := context.WithTimeout(base, timeout)
		reply, cerr := eng.ChatWithOptions(ctx, user, onStep, engine.ChatOptions{ConfirmFunc: confirmFn})
		cancel()

		tr := turnRec{
			Index:             i + 1,
			User:              user,
			Reply:             reply,
			Steps:             steps,
			Confirmations:     confirms,
			ConfirmationCount: len(confirms),
		}
		if cerr != nil {
			tr.ErrorCode = "engine_error"
			tr.ErrorMessage = cerr.Error()
		}
		rec.Turns = append(rec.Turns, tr)
		if reply != "" {
			rec.FinalReply = reply
		}
		if cerr != nil {
			rec.Error = fmt.Sprintf("turn_%d: %v", i+1, cerr)
			break
		}
	}
	return rec
}

// allStepActions collects step.action where step.type ∈ {tool_call, confirm_needed}
// (matches the Python checker's all_step_actions).
func allStepActions(rec *replayCaseRecord) map[string]bool {
	acts := map[string]bool{}
	for _, tr := range rec.Turns {
		for _, st := range tr.Steps {
			if (st.Type == "tool_call" || st.Type == "confirm_needed") && st.Action != "" {
				acts[st.Action] = true
			}
		}
	}
	return acts
}

func behavioralRepoRoot(t *testing.T) string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// This file sits in <root>/cmd, so two Dir steps reach the repo root.
	return filepath.Dir(filepath.Dir(file))
}

func writeReplayJSONL(path string, records []*replayCaseRecord) error {
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for _, rec := range records {
		if err := enc.Encode(rec); err != nil {
			return err
		}
	}
	return nil
}

func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}
