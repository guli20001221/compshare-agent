---
intent: disk_info
source: "2026-05-29 disk-listing routing fix (upstream has no list API; reuse DescribeCompShareInstance.DiskSet)"
examples:
  - question: "我有哪些数据盘"
    plan_json: '{"schema_version":"1.0","intent":"disk_info","slots":{"target_refs":[],"metrics":[],"time_window":null},"confidence":0.85}'
    source: "user-reported phrasing: bare disk inventory question"
  - question: "我的磁盘列表"
    plan_json: '{"schema_version":"1.0","intent":"disk_info","slots":{"target_refs":[],"metrics":[],"time_window":null},"confidence":0.85}'
    source: "alternate phrasing: explicit list verb"
  - question: "uhost-1qyjfcigo1r6 挂了哪些盘"
    plan_json: '{"schema_version":"1.0","intent":"disk_info","slots":{"target_refs":[{"type":"uhost_id_user_input","value":"uhost-1qyjfcigo1r6","source":"user_text","source_span":"uhost-1qyjfcigo1r6"}],"metrics":[],"time_window":null},"confidence":0.85}'
    source: "instance-scoped disk query (target_ref carries UHostId)"
  - question: "我账号下有哪些云盘"
    plan_json: '{"schema_version":"1.0","intent":"disk_info","slots":{"target_refs":[],"metrics":[],"time_window":null},"confidence":0.85}'
    source: "synonym: 云盘 (cloud disk)"
---

# Planner one-shot examples: disk_info intent

Disk-listing questions route to `disk_info` so the renderer foregrounds
`DiskSet[]` from `DescribeCompShareInstance` instead of returning a generic
instance summary. Upstream has no separate disk-list read API.
