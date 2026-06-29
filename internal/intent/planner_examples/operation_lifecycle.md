---
intent: operation_lifecycle
source: "PR1 hotfix (2026-05-28): anchor action-verb chats including ZERO-target so 'help me shutdown' stops drifting to unknown"
examples:
  - question: "帮我关机"
    plan_json: '{"schema_version":"1.0","intent":"operation_lifecycle","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":["DescribeCompShareInstance"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.7}'
    source: "PR1 hotfix Bug 1: ZERO target — engine lists instances and prompts for selection"
  - question: "帮我关机 uhost-1qx1qsw4b1pk"
    plan_json: '{"schema_version":"1.0","intent":"operation_lifecycle","slots":{"target_refs":[{"type":"uhost_id_user_input","value":"uhost-1qx1qsw4b1pk","source":"user_text","source_span":"uhost-1qx1qsw4b1pk"}],"metrics":[],"time_window":null},"required_tools":["DescribeCompShareInstance"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}'
    source: "Batch 1: 关机 + UHostId — direct from 2026-05-28 jitter trace"
  - question: "uhost-test 停了"
    plan_json: '{"schema_version":"1.0","intent":"operation_lifecycle","slots":{"target_refs":[{"type":"uhost_id_user_input","value":"uhost-test","source":"user_text","source_span":"uhost-test"}],"metrics":[],"time_window":null},"required_tools":["DescribeCompShareInstance"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.82}'
    source: "Batch 1: 口语化 '停了' verb — anchors shutdown via colloquial speech"
  - question: "启动 train-gpu"
    plan_json: '{"schema_version":"1.0","intent":"operation_lifecycle","slots":{"target_refs":[{"type":"name","value":"train-gpu","source":"user_text","source_span":"train-gpu"}],"metrics":[],"time_window":null},"required_tools":["DescribeCompShareInstance"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.82}'
    source: "Batch 1: 启动 + Name target_ref — exercises name-typed resolution"
  - question: "把 uhost-xxx 重启一下"
    plan_json: '{"schema_version":"1.0","intent":"operation_lifecycle","slots":{"target_refs":[{"type":"uhost_id_user_input","value":"uhost-xxx","source":"user_text","source_span":"uhost-xxx"}],"metrics":[],"time_window":null},"required_tools":["DescribeCompShareInstance"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.82}'
    source: "Batch 1: 重启 + 口语化 '一下'"
  - question: "给 uhost-1qx1qsw4b1pk 加 200G 数据盘"
    plan_json: '{"schema_version":"1.0","intent":"operation_lifecycle","slots":{"target_refs":[{"type":"uhost_id_user_input","value":"uhost-1qx1qsw4b1pk","source":"user_text","source_span":"uhost-1qx1qsw4b1pk"}],"metrics":[],"time_window":null},"required_tools":["DescribeCompShareInstance"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.82}'
    source: "Batch 1: 加盘 — CreateDiskWorkflow trigger, same intent as start/stop"
  - question: "把 uhost-1qx1qsw4b1pk 保存成镜像"
    plan_json: '{"schema_version":"1.0","intent":"operation_lifecycle","slots":{"target_refs":[{"type":"uhost_id_user_input","value":"uhost-1qx1qsw4b1pk","source":"user_text","source_span":"uhost-1qx1qsw4b1pk"}],"metrics":[],"time_window":null},"required_tools":["DescribeCompShareInstance"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.82}'
    source: "Phase 3: create custom image from an existing instance; workflow asks for Name if missing"
  - question: "保存训练环境，下次复用"
    plan_json: '{"schema_version":"1.0","intent":"operation_lifecycle","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":["DescribeCompShareInstance"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.72}'
    source: "Phase 3: zero-target custom-image request; engine should ask which instance/name rather than guessing"
---

# Planner one-shot examples: operation_lifecycle intent

Lifecycle and configuration commands on existing instances stay anchored to
`operation_lifecycle`, including zero-target requests where the engine should
list instances and ask the user to choose one.
