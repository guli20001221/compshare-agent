---
intent: diagnosis
source: "Stage 2B diagnosis-vs-knowledge boundary"
examples:
  - question: "uhost-abc123 这台启动失败了帮我查"
    plan_json: '{"schema_version":"1.0","intent":"diagnosis","slots":{"target_refs":[{"type":"uhost_id_user_input","value":"uhost-abc123","source":"user_text","source_span":"uhost-abc123"}],"metrics":[],"time_window":null},"confidence":0.85}'
    source: "Stage 2B: concrete instance target stays diagnosis"
  - question: "为什么我开的端口在外面访问不了"
    plan_json: '{"schema_version":"1.0","intent":"diagnosis","slots":{"target_refs":[],"metrics":[],"time_window":null},"confidence":0.85}'
    source: "Stage 2B: no-target port failure report still enters diagnosis; engine asks which instance"
  - question: "跑模型的时候说找不到GPU"
    plan_json: '{"schema_version":"1.0","intent":"diagnosis","slots":{"target_refs":[],"metrics":[],"time_window":null},"confidence":0.85}'
    source: "Stage 2B: no-target GPU failure report still enters diagnosis; engine asks which instance"
  - question: "ssh连接超时一直进不去"
    plan_json: '{"schema_version":"1.0","intent":"diagnosis","slots":{"target_refs":[],"metrics":[],"time_window":null},"confidence":0.85}'
    source: "Stage 2B: no-target SSH failure report still enters diagnosis; engine asks which instance"
  - question: "ssh 连不上进不去"
    plan_json: '{"schema_version":"1.0","intent":"diagnosis","slots":{"target_refs":[],"metrics":[],"time_window":null},"confidence":0.85}'
    source: "G1: SSH cannot-connect phrasing stays diagnosis; engine asks which instance"
---

# Planner one-shot examples: diagnosis intent

When a user mentions a concrete UHost ID + a problem symptom, the planner
must emit `intent=diagnosis` rather than `knowledge_qa`. The Stage 2B
boundary rule: "how do I do X on the platform" → knowledge_qa;
"runtime failure report X" → diagnosis, even when `target_refs` is empty.

## Boundary rule

Without a concrete target ref, default to `knowledge_qa` only for pure usage,
config, how-to, or error-code questions. Runtime symptoms such as SSH timeout,
port unreachable, GPU not found, service unreachable, or init stuck should emit
`diagnosis` with `target_refs: []`; the engine asks which instance later.

## Why a one-shot example anchor

ds-v4-flash without an anchor example sometimes routes "uhost-X 启动失败" to
unknown or knowledge_qa under jitter. No-target symptom phrasings can also be
misread as documentation questions. These anchors pin colloquial problem-report
phrasing to diagnosis without broadening pure how-to questions. G1 adds a
cannot-connect anchor while moving disconnect/how-to phrasing to knowledge_qa.
