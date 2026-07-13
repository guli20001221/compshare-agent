# Program A — the amnesia fix, and what it actually was

Written 2026-07-13. Supersedes the Program A section of
`2026-07-12-amnesia-and-consolidation.md`, whose root-cause was **wrong**.

Everything below is measured on the real **2026-06-26..07-09** chat corpus
(2193 turns / 913 sessions) and on a **443-session / 1454-turn replay of it through the
real HTTP path with `mutating_tools: true`** — production ships mutating on, and read-only
mode *hides* the severity of a misroute (a bad route becomes a polite refusal instead of a
create-instance confirmation card).

---

## The plan's root cause did not reproduce

The 2026-07-12 plan said: the router prompt is saturated with `部署`→`deploy_model`
few-shots, and 「我删了再部署」 becomes a create flow.

**On real traffic it does not.** Of 102 turns containing 部署, the router sent **58 to
`knowledge_qa`** and only 11 to `deploy_model`. Create-family routes on a follow-up are
20/1280 (1.6%), and most read as *correct* (「你直接给我部署好LongCat-Video」). The
few-shot-saturation story is dead. Do not resurrect it.

## What the amnesia actually is — four bugs

### M4 — the keystone. `LastIntent` was never written for the two biggest intents.

`SessionState.LastIntent` is the router's **entire** memory (the transcript is withheld from
its prompt — PR1 hotfix Bug 2, 2026-05-28). It was written in exactly one place:
`recordLastIntentFromPlan`, inside `if handled.Status == HandlerStatusHandled` — i.e. only
when a **direct-dispatch handler** resolved the turn.

`defaultRouteIntents()` (`cmd/trace.go`) is: resource, monitor,
billing_account_unsupported, gpu_specs, stock, pricing, refund_estimate, image_tag_catalog,
model_repository, image_list, net_accelerator.

**`diagnosis` is not in it. `knowledge_qa` is not in it.** Both go to ReAct, both come back
`fallback_ineligible`, so neither could **ever** be "confirmed by a fully-dispatched handler
reply" — and neither was **ever** recorded, no matter how confidently the router classified
them or how many turns the user spent there.

They are **919 of 1280** real follow-up turns. Live replay: a session ran **five consecutive
diagnosis turns with `router_has_last_intent=false` on every one**. The router's only memory
was structurally incapable of holding the two conversations users actually have.

Fix: `rememberLastIntentForRouter` (engine.go, where the code already said "record the
planner intent for all subsequent branches"). **Not** `recordLastIntentFromPlan` — that one
also fires `clearPendingDeployModel` / `clearDeployClarificationCarry`, which at that point
in the turn would run *before* the create/deploy resume logic and tear down the frame it is
about to need.

### M2 — the validator refused users' own instances, then called it `unknown`.

`RegistrySnapshot.ResolveByID` returns `NOT_FOUND_IN_ACCOUNT` for anything it has not seen —
including when it has seen **nothing**, and including when the listing was **paginated**
(`Truncated = TotalCount > len(fetched)`; the live account holds **20 instances and the API
returns 10**). The HTTP path skips `engine.Init()`, so an early-turn registry is empty.

`ErrEntityNotFound` then killed the plan on every retry and the turn shipped as
`intent=unknown`. Real turn: 「我的uhost-1exampleaa01扩的是系统盘吧？」 — the user names their
**own** instance and is told, in effect, that it isn't real.

Fix: `entity.CanAssertAbsence()` — a registry may only assert absence if it synced **cleanly
and completely**. The hallucination guard is untouched: `validateProvenance` still requires
the id to appear verbatim in text the **user** wrote. What is relaxed is the separate, weaker
claim "it exists in the account", which a partial cache was never entitled to make.

### M3 — none of it was diagnosable, which is why it survived.

`LastValidationCode` was computed, used to force `fallback_invalid`, and **thrown away**;
and `ProjectPlannerTrace` **overwrites a rejected plan's intent with `unknown`**. So "the
user asked something off-platform" and "we refused a correct route on a technicality"
produced an identical trace and needed opposite fixes.

`RouterTrace` now carries `validation_code` / `validation_field` / `rejected_intent` /
`attempts` (enum + schema path only, no user text — safe on in production). It paid for
itself immediately. Baseline arm, 1403 real turns:

| | |
|---|---|
| `fallback_invalid`, turn-1 | 3/441 = **0.7%** |
| `fallback_invalid`, follow-up | 36/962 = **3.7%** |
| `unparseable_json` | **0 / 1409** |
| `rejected_intent` | **22 diagnosis**, 10 resource_info, 6 operation_lifecycle, 1 disk_info |

**Not one schema breakdown.** `fallback_invalid` is 100% our own validator refusing routes
the model got right — 39 turns where we threw away a correct answer and replied `unknown`.

### M1 — nothing told the model what its context signals MEAN.

`buildUserPrompt` has always emitted `Last intent:`, `Last selected instance:`, `Last
assistant snippet:`. A grep across the base scaffold, every `route.yaml`
`planner_directives`, the boundary packs and the planner examples found **zero** references
to any of them.

---

## ⚠️ The directive fix was wrong the first time. Read this before touching it.

v1 said: "inherit Last intent when the turn cannot stand alone" and "classify pasted machine
output as Last intent". The **trace metrics loved it** — `unknown` on follow-ups 3.9%→0.0%,
diagnosis derailment 25%→9%, schema_valid up.

A blind, position-randomised judge over **150 real follow-up turns** said it was worth
nothing: **rescued 19, BROKE 11, McNemar exact p=0.20.**

Every breakage had one shape:

    「嗯嗯」                              last=knowledge_qa -> knowledge_qa -> 「当前知识库未覆盖该问题」
    「bash: start_app.sh: No such file」  last=knowledge_qa -> knowledge_qa -> 「当前知识库未覆盖该问题」
    「32」 (answering "how much memory?")  last=deploy_model -> deploy_model -> a create card

**`knowledge_qa` is not an ordinary label.** Its route FORCES a `SearchKnowledge` hop and
then refuses under cite-or-refuse when retrieval is empty — so inheriting it onto a turn
containing no question is a **guaranteed canned refusal**. The create family is the same
shape: its route ends in a confirmation card.

**And `unknown` was never the enemy.** It falls through to ReAct, which carries
`maxHistoryMessages` of conversation — so for a bare 「嗯嗯」 it produces a perfectly good
reply. The metric I gated on counted that *correct* behaviour as amnesia, and would have
rewarded the refusal.

**Two lessons, both expensive:**
1. **Never gate on a metric the treatment produces.** "unknown *while holding* last_intent"
   was my first primary metric — and M4's whole job is to populate `last_intent`, so the
   fixed arm gets a bigger eligible population and the number can improve while behaviour
   degrades.
2. **A route label is not a quality measure.** Score the route's *downstream behaviour*.
   `knowledge_qa` = forced retrieval + refuse-on-empty. Create = confirmation card.
   `unknown`/`diagnosis` = ReAct with full history. The same "misroute" does wildly
   different damage.

v2 makes inheritance **intent-aware**: a pasted error is `diagnosis` **absolutely**;
`knowledge_qa` and the create family are **never inherited**; a bare acknowledgement emits
`unknown` **on purpose**; inheritance is only for continuing a **troubleshoot**.
Re-probed on the exact 11 turns v1 broke: **10 recover**.

The 1 that did not — 「我登录了，怎么去根目录」 — routed to `knowledge_qa` in **both** arms, so it
is a pre-existing cite-or-refuse gap (no Linux-basics content in the KB), **not** caused by
this change. Filed separately.

---

---

## RESULTS — 420 identical real sessions, 914 follow-up turns per arm

Both arms replayed the FULL corpus: **443/443 sessions each, zero throttled turns.** 420
completed cleanly in BOTH arms and are scored (22 dropped for a ws timeout in one arm or the
other — a symmetric exclusion). Real HTTP path. `mutating_tools: true`, as production ships.

### Deterministic. These are COUNTED, not judged.

| metric | baseline | fixed |
|---|---|---|
| `fallback_invalid` on follow-ups | 35/914 = **3.8%** | 5/914 = **0.5%** |
| `fallback_invalid` on turn-1 | 3/420 = 0.7% | **0.0%** |
| **validator rejections of CORRECT routes** | **38** | **5** (−87%) |
| — of which `diagnosis` | **23** | 4 |
| — `resource_info` / `operation_lifecycle` / `disk_info` | 9 / 5 / 1 | 1 / 0 / 0 |
| diagnosis derailment (session opens in diagnosis, then abandons it) | 73/280 = **26.1%** | 49/277 = **17.7%** |
| `last_intent` coverage on follow-ups | 264/914 = **28.9%** | 900/914 = **98.5%** |
| schema_valid, follow-up (**must not regress**) | 95.0% | **98.2%** ↑ |
| schema_valid, turn-1 | 99.3% | **100.0%** |
| `unparseable_json` (the 2026-05-28 avalanche signature) | 0 | **0** |
| transcript in router prompt (`RouterPriorInPrompt`) | 0 | **0** |
| create-family route on a follow-up (mutating ON = a create card) | 3.3% | 2.4% |
| canned non-answers | 3.4% | 4.0% (+0.7 — **inside the noise floor**, see below) |

**33 of the 38 turns where we threw away a correct route are gone.** Twenty-three of them
were users mid-diagnosis being told, in effect, "I don't understand you".

Every gate passes. The only metric that moved the wrong way — canned non-answers, +0.7pt —
is smaller than the measured A/A noise, and is shown below not to be the router's doing.

### ⚠️ THE NOISE FLOOR — read this before believing ANY A/B on this system

Run the **same binary against itself** — same 150 real sessions, second run, same blind judge:

| | run 1 | run 2 |
|---|---|---|
| judge-scored amnesia | 13 (9.1%) | 18 (**12.6%**) |
| discordant verdicts | **27 / 143 = 18.9%** | |
| head-to-head | run 2 "wins" 43% of decided pairs (50% = noise) | |

**Two runs of identical code disagree on 19% of turns and swing the amnesia rate by 3.5
points.** The treatment effect is 1.6–2.8 points. **The noise floor is larger than the
effect.**

So the blind judge (n=499 real follow-up turns, position-balanced, slot skew 3%) reports:

| | baseline | fixed | McNemar |
|---|---|---|---|
| all amnesia | 14.4% | 12.8% | p=0.48 |
| routing amnesia (what the fix targets) | 11.0% | **8.2%** | p=0.11 |
| canned non-answers (pre-existing) | 3.4% | 4.6% | p=0.41 |

— and **none of it is resolvable.** Not at n=499, not at n=900. The apparatus cannot see an
effect this size.

**Two consequences, both important:**

1. **Judge this change on the deterministic table, not the judge.** The bugs are real (code
   + mutation-verified tests), and the counted improvements are large and unambiguous.
2. **This retroactively explains every "dead heat" on this project.** ExA's claim that the
   agent loop cut amnesia 29.7%→8.1%, and the 2026-07-12 38.1%-vs-34.9% result, were almost
   certainly reading this same noise. **Before running another A/B here, run an A/A.**

The +0.7pt canned-non-answer delta is *inside* that floor. And it is not the router's doing:
of the 15 turns where the fixed arm emits a KB refusal and the baseline does not, **all 15
took the SAME route in both arms and ZERO were caused by the routing change.** It is flash's
synthesis sampling against the cite guard — a pre-existing coin flip, ~4–5% of knowledge_qa
turns, in both arms.

### What this means for the amnesia complaint

The router WAS blind, and that is now fixed and provable. But the amnesia users actually
experience is **not primarily a routing failure** — the ReAct loop carries 120 messages of
history and usually rescues a misrouted turn. Decomposing the judge's amnesia flags by
mechanism:

| | baseline | fixed |
|---|---|---|
| lost the thread (generic list / re-ask) | 55 | **41** |
| canned: KB no-evidence refusal | 5 | 13 |
| canned: create card wrongly declined | 8 | 3 |
| canned: round-limit / token-budget | 4 | 7 |

**The next real win is not in the router.** It is in the ~3–5% of turns the system discards
at its own boundaries — the knowledge_qa cite-or-refuse coin flip, the ReAct round cap, the
per-turn token cap. Those throw the whole conversation away, and they fire nondeterministically.

---

## A3 — do NOT add more router context. Closed on the evidence.

The plan reserved A3 for "if A1 is insufficient, give the router a richer structured
conversation-state signal". **Do not do it.** The data says it would be wasted work:

- The router's context is no longer the bottleneck. `last_intent` now reaches **98.5%** of
  follow-up turns (was 28.9%), and the routes it produces are right — validator rejections
  of *correct* routes fell 38 → 5.
- What remains is not a context problem. Decomposing the judge's amnesia flags by mechanism,
  the residue is **canned non-answers** — the `knowledge_qa` cite-or-refuse coin flip, the
  ReAct round cap, the per-turn token cap. Those turns are *correctly routed* and then
  discarded at a budget/guard boundary. No amount of extra signal in the router prompt
  touches them.
- And the ReAct loop already carries 120 messages of history, so it rescues most turns the
  router gets wrong. That is precisely why widening the loop (2026-07-12) did nothing, and
  why enriching the router further would also do little.

**The next fix is the discard boundaries, not the router.** They are enumerated below.

---

## THE NEXT FIX — the discard boundaries (scoped, evidenced, not yet done)

### Guard boundary — code that DELETES a real answer and replaces it with a canned string

Four sites all emit the identical 「当前知识库未覆盖该问题,我无法回答。」 (`cited_guard.go:11`), and
they are **not the same bug**:

| | guard | site | trigger |
|---|---|---|---|
| **G1** | **cited-contract gate** | `engine.go:1651-1661` | the answer contains no `[n]` marker |
| **G2** | **raw-leak guard** | `engine.go:3969-3972` (`guardSearchKnowledgeSynthesis`) | synthesis quoted the evidence too literally |
| G3 | domain-match guard | `engine.go:3973+` | **default OFF** — never fires |
| G4 | terminal-RAG no-evidence | `engine.go:2952-2961` | retrieval genuinely returned zero hits |

**Only G4 is honest.** So which one actually fires on real traffic?

```
BASELINE  24 KB refusals:  21 agent-loop guard (G1/G2) + 3 round-0 gate (G1)   G4: 0
FIXED-v2  26 KB refusals:  26 agent-loop guard (G1/G2)                          G4: 0
```

**ZERO.** In every observed case the agent ran the tools, **retrieved the evidence, and wrote
an answer** — and we deleted it. The message 「知识库未覆盖该问题」 ("the knowledge base does not
cover this") is **false 100% of the time it is shown**. The KB *did* cover it.

And G1's check is (`cited_guard.go:47`):

```go
var numberedCitationRE = regexp.MustCompile(`\[[1-9][0-9]*\]`)
```

**A regex for a square bracket.** A correct, fully-grounded answer that merely failed to type
`[1]` is destroyed. flash omits the bracket nondeterministically — **this is exactly the
~4-5% coin flip measured above** (same route, same query, different sampling; see the A/A
noise floor).

**The defect is the failure ACTION, not the check.** A missing bracket is a *formatting*
violation, and the correct response to bad formatting is to *fix the formatting* — not to
destroy the content and then lie to the user about why. The evidence ledger is already in
hand (`e.searchKnowledgeHitsThisTurn`); attach the citations server-side instead of demanding
a nondeterministic model type them. The existing single cite-retry (`engine.go:1691-1696`)
does not fix this — it just re-asks the same model to please remember a bracket.

### Budget boundary — the loop runs out mid-conversation

| cap | value | message |
|---|---|---|
| `maxReActRounds` | **10** (`engine.go:43`) | 「抱歉，处理轮次超限，请重新描述您的需求。」 (`engine.go:1834`) |
| `max_tokens_per_turn` | **200000** (config) | 「本次问题消耗的算力已超过单次上限，请简化问题或拆分提问。」 (`engine.go:102`) |
| `maxReadExpensiveCallsPerTurn` | 20 (`engine.go:71`) | tool budget |

Each of these throws the **entire conversation** away, which is exactly what a user
experiences as amnesia — and they fire nondeterministically, which is why no router work
moves them.

## A4 — the cutover, decided

`DIRECT_DISPATCH_INTENTS=off` is **not** the amnesia fix, and after M4 it is not even coupled
to it: `rememberLastIntentForRouter` runs *before* dispatch, so `LastIntent` is recorded
whatever the dispatch set contains (`TestARouterOnlyIntentIsStillRememberedForTheNextTurn`
proves it — it routes `diagnosis` while `EnabledIntents=[pricing_query]`). Decide the
cutover on latency/quality grounds, separately.

## Harness traps that cost real time — do not repeat

1. **`governance.normalizeLimits`: a limit of `0` restores the DEFAULT, it does not
   uncap.** So `llm_qps: 0` silently means `llm_qps: 5`. The first run started answering
   real user turns with 「请求过于频繁」 — throttled turns that would have been scored as
   routing data. Set explicit large numbers. Only `user_turn_*` honours 0.
2. **The replay's default identity (top_org 2384301) has no STS role** → every API call
   fails with `AssumeRole RetCode=11277`, the reply becomes 「查询暂时失败」, and **the server
   log stays clean**. Use `-top-org 66391350 -org 64404856`. Always single-turn smoke for
   REAL data before a long run.
3. **Compare only sessions COMPLETE AND CLEAN IN BOTH arms.** A session appears in the trace
   dir on its *first* turn, so a still-running one contributes a truncated turn list; and a
   ws timeout aborts a session mid-way. Either one silently deflates a denominator.
4. **A validator you wrote is a hypothesis.** My over-inheritance checker flagged the fixed
   arm as "stuck" on 「云硬盘仍会按量计费是什么意思，可以一起帮我关闭吗」 — but the sentence's action
   clause is 「可以一起帮我关闭吗」, so `operation_lifecycle` was right and the fixed reply was
   strictly *better*. The checker was wrong, not the model.
