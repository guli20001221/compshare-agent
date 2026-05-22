---
intent: diagnosis
source: "Stage 2B diagnosis-vs-knowledge boundary"
examples:
  - question: "uhost-abc123 这台启动失败了帮我查"
    plan_json: '{"schema_version":"1.0","intent":"diagnosis","slots":{"target_refs":[{"type":"uhost_id_user_input","value":"uhost-abc123","source":"user_text","source_span":"uhost-abc123"}],"metrics":[],"time_window":null},"required_tools":["DescribeCompShareInstance"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}'
    source: "Stage 2B: concrete instance target stays diagnosis"
---

# Planner one-shot examples: diagnosis intent

When a user mentions a concrete UHost ID + a problem symptom, the planner
must emit `intent=diagnosis` rather than `knowledge_qa`. The Stage 2B
boundary rule: "how do I do X on the platform" → knowledge_qa;
"my specific instance has problem X" with `target_refs` → diagnosis.

## Boundary rule

Without a concrete target ref, default to `knowledge_qa` for usage,
config, or error-code questions. Diagnosis must carry an instance target.

## Why a one-shot example anchor

ds-v4-flash without an anchor example sometimes routes "uhost-X 启动失败" to
unknown or knowledge_qa under jitter. The anchor pins the colloquial
problem-report phrasing to diagnosis.
