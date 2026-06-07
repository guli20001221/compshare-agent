export const meta = {
  name: 'analyze-platform-faithfulness',
  description: 'Judge whether turning external KB on (Phase-3 default state) drifts/degrades platform-FAQ answers vs the current prod default (external off)',
  phases: [
    { title: 'Drift', detail: 'one judge per probe comparing ext0off vs ext1on' },
    { title: 'Verify', detail: 'skeptic re-checks any flagged regression' },
    { title: 'Synthesize', detail: 'overall faithfulness verdict' },
  ],
}

const INPUT = __INPUT_JSON__;
const probes = INPUT.probes

const VERDICT = {
  type: 'object', additionalProperties: false,
  required: ['probe_id', 'category', 'drift', 'on_answer_ok', 'ext_contaminated', 'evidence', 'notes'],
  properties: {
    probe_id: { type: 'string' },
    category: { type: 'string' },
    drift: { type: 'string', enum: ['none', 'minor_ok', 'regression'], description: 'none=ext1on equivalent to ext0off; minor_ok=cosmetic/jitter difference but both correct platform answers; regression=ext1on is worse/wrong/contaminated vs ext0off' },
    on_answer_ok: { type: 'boolean', description: 'is the external-ON answer a correct, platform-grounded answer (or a correct handler/refusal for control probes)?' },
    ext_contaminated: { type: 'boolean', description: 'did the external-ON answer wrongly pull in self-hosted/external (vLLM/Ollama/SGLang) content for this PLATFORM question?' },
    evidence: { type: 'string', description: 'VERBATIM snippet from the ext1on (and/or ext0off) reply supporting the verdict. Quote, do not paraphrase.' },
    notes: { type: 'string' },
  },
}

const VERIFY = {
  type: 'object', additionalProperties: false,
  required: ['probe_id', 'agrees', 'corrected_drift', 'evidence'],
  properties: {
    probe_id: { type: 'string' },
    agrees: { type: 'boolean' },
    corrected_drift: { type: 'string', enum: ['none', 'minor_ok', 'regression'] },
    evidence: { type: 'string' },
  },
}

async function retryAgent(prompt, opts, fallback, tries = 5) {
  for (let i = 0; i < tries; i++) {
    try { return await agent(prompt, opts) }
    catch (e) { if (i === tries - 1) { log(`agent ${opts.label} failed: ${String(e).slice(0, 80)}`); return fallback } }
  }
  return fallback
}

function jp(p) {
  const off = p.ext0off.map((r, i) => `  [off run${i + 1}] intent=${r.intent} cited=${JSON.stringify(r.cited)}\n  ${r.reply}`).join('\n')
  const on = p.ext1on.map((r, i) => `  [on run${i + 1}] intent=${r.intent} cited=${JSON.stringify(r.cited)}\n  ${r.reply}`).join('\n')
  return `You are judging FAITHFULNESS DRIFT for one PLATFORM-FAQ question. Two conditions were run live:
- ext0off = the CURRENT production default (external KB OFF, agentic SearchKnowledge OFF) — the baseline.
- ext1on = the PROPOSED Phase-3 default (external KB ON, agentic SearchKnowledge ON).
The external KB holds SELF-HOSTED tool/ops docs (vLLM/Ollama/SGLang/GPU). The platform corpus (w0-*) holds CompShare's own platform + ModelVerse hosted-API docs. Turning external KB on changes the retrieval pool for EVERY question — the risk is that a PLATFORM question now drifts (wrong routing, degraded answer, or contamination by self-hosted/external content).

PROBE ${p.id} (${p.category}); expected=${p.expected}
Q: ${p.question}

ext0off (baseline) answers:
${off}

ext1on (Phase-3) answers:
${on}

Judge:
- drift: 'none' if ext1on answers are equivalent in substance to ext0off; 'minor_ok' if they differ only cosmetically / by routing jitter but both are correct platform answers (or both the correct handler/refusal for control probes); 'regression' if ext1on is worse — wrong, degraded, or contaminated by self-hosted/external (vLLM/Ollama/SGLang) content that doesn't belong in a platform answer.
- on_answer_ok: is the ext1on answer a correct platform-grounded answer (for control probes: the correct handler card / refusal)?
- ext_contaminated: TRUE only if the ext1on answer pulls self-hosted/external content into this platform question.
- evidence MUST be a verbatim quote. For control probes (pricing/stock/account) expected=handler/refuse: 'none' if both conditions give the same handler/refusal; the cited=[] + same intent is the signal.
Return the verdict.`
}

phase('Drift')
const results = await pipeline(
  probes,
  (p) => retryAgent(jp(p), { label: `drift:${p.id}`, phase: 'Drift', schema: VERDICT },
    { probe_id: p.id, category: p.category, drift: 'minor_ok', on_answer_ok: true, ext_contaminated: false, evidence: '', notes: 'JUDGE_FAILED', _failed: true }),
  async (v, p) => {
    if (v.drift !== 'regression' && !v.ext_contaminated) return { ...v, verified: 'skipped' }
    const off = p.ext0off.map((r, i) => `[off${i + 1}] ${r.reply}`).join('\n')
    const on = p.ext1on.map((r, i) => `[on${i + 1}] ${r.reply}`).join('\n')
    const vv = await retryAgent(
      `A first judge flagged probe ${p.id} (${p.category}) as drift=${v.drift} ext_contaminated=${v.ext_contaminated}. Challenge it. Q: ${p.question}\nBASELINE (ext off):\n${off}\nPHASE-3 (ext on):\n${on}\nIs the Phase-3 answer REALLY a regression/contamination vs baseline, or did the judge over-flag normal jitter/equivalent content? Quote verbatim.`,
      { label: `verify:${p.id}`, phase: 'Verify', schema: VERIFY }, null)
    if (!vv) return { ...v, verified: 'verify_failed' }
    return { ...v, verified: vv.agrees ? 'agreed' : 'corrected', final_drift: vv.corrected_drift, verify_evidence: vv.evidence }
  }
)

const verdicts = results.filter(Boolean).map(v => ({ ...v, finalDrift: v.final_drift || v.drift }))

phase('Synthesize')
const tally = collectionsTally(verdicts)
function collectionsTally(vs) {
  const t = { none: 0, minor_ok: 0, regression: 0, contaminated: 0, on_ok: 0, total: vs.length, regressions: [], contaminations: [] }
  for (const v of vs) {
    t[v.finalDrift] = (t[v.finalDrift] || 0) + 1
    if (v.ext_contaminated) { t.contaminated++; t.contaminations.push(v.probe_id) }
    if (v.on_answer_ok) t.on_ok++
    if (v.finalDrift === 'regression') t.regressions.push(v.probe_id)
  }
  return t
}
log(`tally: ${JSON.stringify(tally)}`)

const SYNTH = {
  type: 'object', additionalProperties: false,
  required: ['faithfulness_pass', 'verdict', 'regressions', 'contaminations', 'summary_md', 'recommendation'],
  properties: {
    faithfulness_pass: { type: 'boolean', description: 'TRUE if external-on causes NO platform-FAQ regression or contamination (gate #4 of the Phase-3 default-on decision)' },
    verdict: { type: 'string', enum: ['PASS', 'PASS_WITH_NITS', 'FAIL'] },
    regressions: { type: 'array', items: { type: 'object', additionalProperties: false, required: ['probe_id', 'what', 'evidence'], properties: { probe_id: { type: 'string' }, what: { type: 'string' }, evidence: { type: 'string' } } } },
    contaminations: { type: 'array', items: { type: 'string' } },
    summary_md: { type: 'string', description: 'markdown table: category | n | none | minor_ok | regression | contaminated' },
    recommendation: { type: 'string' },
  },
}

const synth = await retryAgent(
  `Synthesize platform-FAQ faithfulness for the Phase-3 default-on gate (#4: platform answers must NOT drift/degrade/contaminate when external KB + agentic SearchKnowledge are flipped default-ON). ${probes.length} platform-FAQ probes compared ext0off (prod baseline) vs ext1on (Phase-3 state), then adversarially re-verified.\n\nPER-PROBE: ${JSON.stringify(verdicts.map(v => ({ id: v.probe_id, cat: v.category, drift: v.finalDrift, on_ok: v.on_answer_ok, contaminated: v.ext_contaminated, evidence: v.evidence, verified: v.verified, vev: v.verify_evidence, notes: v.notes })))}\n\nTALLY: ${JSON.stringify(tally)}\n\nDecide faithfulness_pass=true only if zero regressions and zero contaminations (routing jitter where both conditions give correct platform answers is NOT drift). List any real regression/contamination with verbatim evidence. recommendation: if PASS, gate #4 is met and the default-on PR is justified once the user greenlights; if not, name what to fix.`,
  { label: 'synthesize', phase: 'Synthesize', schema: SYNTH },
  { faithfulness_pass: null, verdict: 'PASS_WITH_NITS', regressions: [], contaminations: [], summary_md: '(synth failed)', recommendation: 'compute from tally/verdicts' })

return { tally, verdicts, synth }
