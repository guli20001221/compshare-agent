package engine

import (
	"context"
	"strings"

	"github.com/compshare-agent/internal/entity"
)

// diagnosisResolutionSnapshot returns a registry the deterministic instance-reference
// scan can actually work against.
//
// callPlannerOnce hands the plan the CACHED snapshot (e.RegistrySnapshot()), and the HTTP
// path skips engine.Init() — so an early-turn registry is empty. entity derives the id
// prefixes it scans for FROM the instances it holds (RegistrySnapshot.instanceIDPrefixes),
// so an empty registry cannot recognise "uhost-…" as an instance reference at all: the user
// names their machine and we see nothing there to name.
//
// CanAssertAbsence asks exactly the question that matters here — "is this registry in a
// position to tell me this instance is not the user's?" — so the refresh is gated on it. A
// complete, fresh registry is left alone; the call is TTL-cached either way. Going to look
// costs one read. Not looking costs the user a 「请问是哪台实例出了问题？」 about the machine
// they just named.
func (e *Engine) diagnosisResolutionSnapshot(ctx context.Context, cached entity.RegistrySnapshot) entity.RegistrySnapshot {
	if cached.CanAssertAbsence() {
		return cached
	}
	fresh, ok, err := e.freshResourceSelectionSnapshot(ctx, cached)
	if err != nil || !ok {
		return cached
	}
	return fresh
}

// rememberUserNamedInstance writes down the instance the USER named, in their own words, on
// a lane that has no direct-dispatch handler to write it down for them.
//
// SessionState.SelectedInstanceID — the field refreshSystemPrompt renders as
// 「当前会话已选实例：…」 and the router reads back as LastSelectedInstanceID — is written in
// exactly two places today:
//
//   - the direct-dispatch lane (recordSelectedInstanceID / recordSelectedInstanceFromEnvelope,
//     from deterministic_targets.go and tryRouteDispatch), and
//   - recordToolFacts, and only when a tool result happens to carry exactly ONE host
//     (recordInstanceStateFacts).
//
// DIAGNOSIS SATISFIES NEITHER. It has no direct-dispatch handler — it falls through to ReAct —
// and its Diagnose* tools are not in recordToolFacts' action switch at all. So a turn where the
// user typed 「我的 uhost-… SSH 连不上」 diagnosed that exact instance and then forgot which one it
// was. The next turn's 「还是不行」 arrives at a system prompt with no bound instance, and the
// agent asks the user to identify a machine they already named.
//
// This is the same structural hole rememberLastIntentForRouter closed one field over (see its
// comment in engine.go): state only the direct-dispatch lane knew how to write, on the two
// intents that never go through it. That fix remembered WHICH CONVERSATION. Nobody remembered
// WHICH MACHINE.
//
// Measured on 2026-06-26..07-09 production (1161 sessions / 2659 assistant messages): of the 22
// turns where the agent asked 「请问是哪台实例出了问题？」, 6 already had an instance id on the
// record — and in 5 of those the USER had typed it themselves. All 22 were the model asking
// mid-ReAct; the canned clarification constants never fired at all (the agentic-search escape
// hatch returns before them).
//
// EVIDENCE STANDARD. This deliberately does NOT read plan.Slots.TargetRefs. TargetRef.Source is
// a field the MODEL fills in and can simply assert as "user_text" — validateProvenance exists
// because of that. The referent is instead re-derived here by a deterministic scan of the user's
// literal message against the registry, so no model output can reach this binding. Exactly one
// resolved referent and nothing left unresolved, or nothing is recorded.
//
// TRUST. It records with SelectedInstanceSourceUser, because that is what the evidence IS: the
// user named it. That is already what an identical reference in a monitor / resource / lifecycle
// turn records (recordSelectedInstanceFromEnvelope; deterministic_targets.go) — diagnosis was
// the arbitrary exception, not the rule. The consequence is real and intended: a later 「关掉它」
// inside the 1800s TTL now resolves, exactly as it already does after 「uhost-… 的监控」. It does
// NOT reopen phantom selection: workflowTargetIsTrusted exists to stop the MODEL electing a
// target out of a list, and no model output is consulted anywhere on this path.
func (e *Engine) rememberUserNamedInstance(userMsg string, snapshot entity.RegistrySnapshot) {
	if strings.TrimSpace(userMsg) == "" {
		return
	}
	hits, unresolved := snapshot.ResolveInstanceRefsInText(userMsg)
	if len(unresolved) > 0 {
		// The user typed something id-shaped that this registry cannot resolve. Silence is
		// the right answer: a typo must never silently bind the session to a different machine.
		return
	}
	switch {
	case len(hits) == 1 && hits[0] != nil:
		e.recordSelectedInstanceID(hits[0].UHostId, hits[0].Name)
		return
	case len(hits) > 1:
		// Two machines named in one breath is ambiguity, not evidence. Let the turn ask.
		return
	}
	if inst, ok := findUniqueInstanceNameInText(userMsg, snapshot); ok {
		e.recordSelectedInstanceID(inst.UHostId, inst.Name)
	}
}
