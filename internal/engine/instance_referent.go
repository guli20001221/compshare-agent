package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/readprojection"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
)

// The current instance referent is read-side state, not semantic history: it
// names what the conversation is currently about. It never grants write
// authority — every mutating operation still resolves and verifies its own
// target. Recording happens only from an actual tool observation or a
// confirmed write, never from a turn-entry scan of the user's text.

// recordObservedInstanceFromTool keeps the one piece of read-side state that is
// not semantic history: the current live instance reference used by the target
// binder. It never grants write authority; confirmation and live revalidation
// still decide whether an operation may execute.
func (e *Engine) recordObservedInstanceFromTool(action string, result *tools.SafeToolResult) {
	if !e.sessionStateHydrated || result == nil || result.RawResult == nil {
		return
	}
	switch action {
	case "DescribeCompShareInstance":
		e.recordObservedInstanceFromDescribe(result.RawResult)
	case "GetCompShareInstanceMonitor":
		e.recordObservedInstanceFromMonitor(result.RawResult)
	}
}

func (e *Engine) recordObservedInstanceFromDescribe(raw map[string]any) {
	hosts, _ := raw["UHostSet"].([]any)
	if len(hosts) != 1 {
		return
	}
	// Exactly one host in the result = the turn unambiguously concerns that
	// instance for read-only follow-up context. A multi-host (list-all) result
	// is ambiguous and must NOT set it. Tool-observed selections are not trusted
	// as write targets; the write-target dual-proof verifier only accepts a
	// genuine user selection (observed != chosen).
	if row, ok := hosts[0].(map[string]any); ok {
		if snap := entity.InstanceFromMap(row); snap.UHostId != "" {
			e.recordObservedInstanceID(snap.UHostId, snap.Name)
		}
	}
}

func (e *Engine) recordObservedInstanceFromMonitor(raw map[string]any) {
	scalars := readprojection.ExtractMonitorScalars(raw, nil)
	if len(scalars) == 0 {
		return
	}
	bySubject := make(map[string]struct{}, len(scalars))
	for _, s := range scalars {
		if s.SubjectID == "" {
			continue
		}
		bySubject[s.SubjectID] = struct{}{}
	}
	// A monitor query scoped to exactly one instance = that instance is the
	// one under discussion → track it for cross-turn reference resolution.
	if len(bySubject) == 1 {
		for subjectID := range bySubject {
			e.recordObservedInstanceID(subjectID, "")
		}
	}
}

// clearSelectedInstance removes conversation target authority while preserving
// unrelated session continuity (background jobs, agent cursor, evidence and any
// still-useful displayed selection list).
func (e *Engine) clearSelectedInstance() {
	if e == nil || !e.sessionStateHydrated {
		return
	}
	e.sessionState.SelectedInstanceID = ""
	e.sessionState.SelectedInstanceName = ""
	e.sessionState.SelectedInstanceSource = ""
	e.sessionState.SelectedInstanceAtUnix = 0
	e.sessionState.SelectedInstanceFreshness = ""
	e.sessionState.SchemaVersion = SessionStateSchemaCurrent
}

func (e *Engine) clearSelectedInstanceIfMatches(id string) {
	if strings.EqualFold(strings.TrimSpace(e.sessionState.SelectedInstanceID), strings.TrimSpace(id)) {
		e.clearSelectedInstance()
	}
}

// recordObservedInstanceID records one instance as read-only conversational
// context from a tool result. It is intentionally weaker than the user_selected
// record made after an approved confirmation card: an observation may help the
// Agent understand who "它" is, but it grants NO execution authority. A write is
// authorized by Request* -> Resolver -> the confirmation gate, and the sealed
// contract guarantees what executes is what was confirmed.
func (e *Engine) recordObservedInstanceID(id, name string) {
	e.recordSelectedInstanceIDWithSource(id, name, SelectedInstanceSourceObserved)
}

// recordCreatedInstanceReferent makes the sole result of a confirmed create the
// conversation's current instance. It reuses the existing user-selected
// continuity record: this is a referent for "刚创建的那台", not permission to
// enter or mutate it — those paths still revalidate the target and show their
// own confirmation card. A multi-instance result remains unselected because the
// server has no basis for choosing one of them.
func (e *Engine) recordCreatedInstanceReferent(action string, params map[string]any, result *workflow.Result) {
	if action != "CreateInstanceWorkflow" || result == nil || !result.Success {
		return
	}
	ids := committedInstanceIDs(result)
	if len(ids) != 1 {
		return
	}
	id := ids[0]
	name := ""
	observed, _ := result.Data["Observed"].([]map[string]any)
	for _, row := range observed {
		if strings.EqualFold(strings.TrimSpace(fmt.Sprint(row["UHostId"])), id) {
			name = strings.TrimSpace(fmt.Sprint(row["Name"]))
			if name == "<nil>" {
				name = ""
			}
			break
		}
	}
	if name == "" {
		name = strings.TrimSpace(fmt.Sprint(params["Name"]))
		if name == "<nil>" {
			name = ""
		}
	}
	e.recordSelectedInstanceIDWithSource(id, name, SelectedInstanceSourceUser)
}

func (e *Engine) recordSelectedInstanceIDWithSource(id, name, source string) {
	if !e.sessionStateHydrated || id == "" {
		return
	}
	// Observing the instance the user already chose does not un-choose it: an
	// observed record must never replace a genuine user selection, even when the
	// read happened to inspect another instance. A passive Describe is evidence,
	// not the user switching targets. It may fill a missing name for the same id,
	// but it does not rewrite provenance, timestamp, or freshness.
	if source == SelectedInstanceSourceObserved &&
		e.sessionState.SelectedInstanceSource == SelectedInstanceSourceUser {
		if e.sessionState.SelectedInstanceID == id && e.sessionState.SelectedInstanceName == "" && name != "" {
			e.sessionState.SelectedInstanceName = name
			e.sessionState.SchemaVersion = SessionStateSchemaCurrent
		}
		return
	}
	if name == "" {
		if inst, res := e.RegistrySnapshot().ResolveByID(id); res.Status == entity.ResolveHit && inst != nil {
			name = inst.Name
		}
		// Re-recording the SAME instance with no name in hand — an authorized explicit
		// SSH target or a confirmed platform-write target — must not blank a name the session
		// already knew. A rehydrated or post-mutation registry is cold and resolves
		// nothing, and the context card would then be able to name the box only by
		// id, which reads to the user as the agent having forgotten it.
		if name == "" && e.sessionState.SelectedInstanceID == id {
			name = e.sessionState.SelectedInstanceName
		}
	}
	e.sessionState.SelectedInstanceID = id
	e.sessionState.SelectedInstanceName = name
	e.sessionState.SelectedInstanceSource = source
	e.sessionState.SelectedInstanceAtUnix = time.Now().Unix()
	e.sessionState.SelectedInstanceFreshness = ContinuityFreshnessFresh
	e.sessionState.SchemaVersion = SessionStateSchemaCurrent
}

// selectedInstanceTTLSeconds bounds passively observed target hints. A stamped
// user_selected target is conversation-scoped and remains bindable until the
// user explicitly selects another target.
const selectedInstanceTTLSeconds = 1800

// expireStaleSelectedInstance classifies the carried instance at turn entry.
// Observed hints expire after selectedInstanceTTLSeconds. A stamped
// user_selected target becomes stale for observability but never expired merely
// because time passed: same-conversation continuity ends only when the user
// selects another target. An unstamped legacy row remains expired because the
// current state shape cannot prove how it was selected.
func (e *Engine) expireStaleSelectedInstance(now time.Time) {
	if strings.TrimSpace(e.sessionState.SelectedInstanceID) == "" {
		return
	}
	at := e.sessionState.SelectedInstanceAtUnix
	if at <= 0 {
		e.sessionState.SelectedInstanceFreshness = ContinuityFreshnessExpired
		e.sessionState.SchemaVersion = SessionStateSchemaCurrent
		return
	}
	freshness := continuityFreshness(at, selectedInstanceTTLSeconds, now)
	if e.sessionState.SelectedInstanceSource == SelectedInstanceSourceUser {
		if freshness == ContinuityFreshnessExpired {
			freshness = ContinuityFreshnessStale
		}
		e.sessionState.SelectedInstanceFreshness = freshness
		e.sessionState.SchemaVersion = SessionStateSchemaCurrent
		return
	}
	if freshness == ContinuityFreshnessExpired {
		e.sessionState.SelectedInstanceFreshness = ContinuityFreshnessExpired
		e.sessionState.SchemaVersion = SessionStateSchemaCurrent
		return
	}
	e.sessionState.SelectedInstanceFreshness = freshness
}

func (e *Engine) markRegistryInvalidated(action string) {
	if e.registry == nil {
		return
	}
	e.registry.MarkInvalidated(action)
}
