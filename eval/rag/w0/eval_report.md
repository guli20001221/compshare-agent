# Stage 2B W0 Eval Report - 2026-05-16

Verdict: **PASS**
Deploy: **WRITTEN**
Failed gates: None

## Inputs
- Chunks: `<runs>/rag-11a-1-20260516/phase2-codex/stage2b_w0.merged.jsonl` (121 chunks)
- Questions: `<runs>/rag-11a-1-20260516/phase2-codex/golden_questions.jsonl` (269 questions)

## Gates
- Pillar 0 no tautology: 0 violations - PASS
- Anchor Top-3 hit rate: 100.0% (15/15) - PASS
- Distribution Top-3 hit rate: 79.8% (threshold 60.0%) - PASS
- Safety failures: 0 - PASS
- Internal leakage: 0 flagged chunks - PASS
- Answer faithfulness: 98.4% (threshold 95.0%) - PASS
- Answer citation rate: 100.0% over non-refusal answers (threshold 100.0%) - PASS
- Fabricated rate: 0.0% (threshold 0.0%) - PASS

## Retrieval
- Evaluated answer questions: 257
- Excluded non-answer questions: 12
- Failed retrieval questions: 52
- Failed anchor question ids: None
- Per-group hit rate:
  - `billing_mode_shutdown`: 81.2% (13/16)
  - `cuda_nvidia_driver`: 100.0% (11/11)
  - `image_types_gpu_specs`: 75.9% (22/29)
  - `invoice_refund_arrears`: 90.6% (29/32)
  - `modelverse_package_credit`: 75.6% (99/131)
  - `monitor_init_failure`: 81.8% (9/11)
  - `remote_login_ssh_jupyter`: 88.9% (8/9)
  - `windows_rdp_sound`: 77.8% (14/18)

## Answer Judge
- Answer questions evaluated: 257
- Refusal answers: 204
- Non-refusal citation denominator: 53
- Grounded rate: 98.4%
- Citation rate: 100.0%
- Fabricated rate: 0.0%
- Answer model: `deepseek-v4-pro`
- Judge model: `claude-opus-4-7`

## Corpus Distribution
- `billing_rule`: 23
- `driver_cuda`: 5
- `image`: 12
- `init_failure`: 3
- `login`: 7
- `modelverse`: 62
- `monitor`: 2
- `resource_purchase`: 2
- `windows`: 5

## Question Distribution
- `answer`: 257
- `escalate`: 3
- `hard_block`: 5
- `refuse`: 4

## Outstanding needs_split Queue
- None

## needs_review Queue
- Candidate chunks carrying low-confidence labels: 0

## Failed Answers
- `w0-golden-0120`: judge_flagged
- `w0-golden-0196`: judge_flagged
- `w0-golden-0197`: judge_flagged
- `w0-golden-0204`: judge_flagged

## Internal Leakage Findings
- None
