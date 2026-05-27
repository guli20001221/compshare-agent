---
intent: knowledge_qa
source: "Stage 2B + PR #34a/#52/#60 knowledge_qa routing regressions + R3-A1 modelverse model-API coverage"
compact: true
examples:
  - question: "为啥显卡内存满了 GPU 占用才 10%"
    plan_json: '{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}'
    source: "PR #60: concept question with monitor-trigger words"
  - question: "how do I issue an invoice"
    plan_json: '{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}'
    source: "PR #52: finance process question, not personal status"
  - question: "what image types does the platform provide"
    plan_json: '{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}'
    source: "Stage 2B: platform concept question"
  - question: "远程桌面没声音该怎么处理"
    plan_json: '{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}'
    source: "Stage 2B: platform how-to/config boundary"
  - question: "错误码 226601 是什么意思"
    plan_json: '{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}'
    source: "Stage 2B: error-code knowledge question"
  - question: "Linux 怎么装 NVIDIA 驱动"
    plan_json: '{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}'
    source: "Stage 2B: platform how-to/config boundary"
  - question: "Coding Plan 的 BaseURL 应该填什么"
    plan_json: '{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}'
    source: "Stage 2B: model API configuration"
  - question: "怎么在 VSCode 里连 GPU 实例"
    plan_json: '{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}'
    source: "Stage 2B: connection how-to"
  - question: "在 CLINE 里加 mcp-server-sqlite 那段 json 该怎么写"
    plan_json: '{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}'
    source: "PR #60: third-party tool configuration jargon"
  - question: "怎么查我这个月的账单"
    plan_json: '{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}'
    source: "PR #52: billing navigation question"
  - question: "哪里可以看发票发起记录"
    plan_json: '{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}'
    source: "PR #52: invoice navigation question"
  - question: "包月和按量哪个划算"
    plan_json: '{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}'
    source: "PR #34a: platform comparison question"
  - question: "实例磁盘可以扩容吗"
    plan_json: '{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}'
    source: "PR #34a: platform feasibility question"
  - question: "退款流程是怎样的"
    plan_json: '{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}'
    source: "PR #34a: platform procedure question"
  - question: "Suno 怎么用 API 生成歌曲"
    plan_json: '{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}'
    source: "R3-A1: modelverse music-gen API (Suno)"
  - question: "Vidu 接口怎么传图生成视频"
    plan_json: '{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}'
    source: "R3-A1: modelverse video-gen API (Vidu)"
  - question: "flux 模型调用 API 怎么传 prompt 和图"
    plan_json: '{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}'
    source: "R3-A1: modelverse image-gen API (flux)"
  - question: "用 OpenAI SDK 调 gpt-image-1 怎么传参"
    plan_json: '{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}'
    source: "R3-A1: modelverse OpenAI-compat API (gpt-image)"
  - question: "minimax-speech 怎么生成中文语音"
    plan_json: '{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}'
    source: "R3-A1: modelverse TTS API (minimax-speech)"
  - question: "modelverse 返回 1002 是什么错误"
    plan_json: '{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}'
    source: "R3-A1: modelverse error-code reference"
---

# Planner one-shot examples: knowledge_qa intent

14 anchors covering platform how-to/config, error-codes, FAQ/process,
billing navigation, comparison/feasibility/procedure phrasings, and
concept questions that contain monitor-trigger or finance-trigger
keywords without actually being monitor/billing requests.

## Why this many anchors

knowledge_qa is the "default" intent for platform questions without a
concrete instance target — it's the routing fallback the planner picks
when no more-specific intent matches. ds-v4-flash classification has
historically regressed at three boundaries:

1. **concept with trigger words** (PR #60): "为啥显卡内存满了 GPU 占用才 10%"
   contains "GPU 占用" which is a monitor trigger; without an anchor
   the planner emits monitor_query and the engine reaches for tools
   the user did not ask for.
2. **finance process vs personal status** (PR #52): "how do I issue an
   invoice" / "怎么查我这个月的账单" / "哪里可以看发票发起记录" are
   navigation/process questions the docs answer; without anchors the
   planner routes them to billing_account_unsupported and refuses.
3. **platform comparison/feasibility/procedure** (PR #34a): "包月和
   按量哪个划算" / "实例磁盘可以扩容吗" / "退款流程是怎样的" — yes/no
   or comparison questions about platform usage that don't reference
   a specific instance. Without anchors the planner sometimes emits
   billing_instance or unknown.

## Migration notes (C5 Phase B)

This file was migrated from an inline Go literal in
`internal/intent/planner.go` (group at lines 265-340 pre-migration).
The disk-backed `parsePlannerExampleFrontmatter` loader emits a
`plannerPromptExampleGroup` byte-equal to the previous Go literal.
The contract is enforced by `planner_examples_test.go`:
`TestPlannerExamples_KnowledgeQADiskLoaderEqualsLegacy` and the
`TestPlannerExamples_FullSystemPromptStable` SHA hash. Editing this
file MUST keep the SHA stable unless the change is an intentional
prompt change requiring an SHA bump with justification.
