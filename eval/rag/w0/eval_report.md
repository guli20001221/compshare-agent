# Stage 2B W0 Eval Report - 2026-05-15

Verdict: **PASS**
Deploy: **WRITTEN**
Failed gates: None

## Inputs
- Chunks: `<runs>/rag-w0-pr-rag-4i-20260514-codex/chunks/chunks_w0.candidate.jsonl` (62 chunks)
- Questions: `F:/compshare-agent-worktrees/rag-5/eval/rag/w0/golden_questions.jsonl` (146 questions)

## Gates
- Pillar 0 no tautology: 0 violations - PASS
- Anchor Top-3 hit rate: 100.0% (10/10) - PASS
- Distribution Top-3 hit rate: 86.6% (threshold 60.0%) - PASS
- Safety failures: 0 - PASS
- Internal leakage: 0 flagged chunks - PASS
- Answer faithfulness: 97.8% (threshold 80.0%) - PASS
- Answer citation rate: 100.0% over non-refusal answers (threshold 100.0%) - PASS
- Fabricated rate: 0.0% (threshold 0.0%) - PASS

## Retrieval
- Evaluated answer questions: 134
- Excluded non-answer questions: 12
- Failed retrieval questions: 18
- Failed anchor question ids: None
- Per-group hit rate:
  - `billing_mode_shutdown`: 81.8% (9/11)
  - `cuda_nvidia_driver`: 100.0% (11/11)
  - `image_types_gpu_specs`: 84.0% (21/25)
  - `invoice_refund_arrears`: 83.3% (5/6)
  - `modelverse_package_credit`: 83.7% (36/43)
  - `monitor_init_failure`: 90.9% (10/11)
  - `remote_login_ssh_jupyter`: 88.9% (8/9)
  - `windows_rdp_sound`: 88.9% (16/18)

## Answer Judge
- Answer questions evaluated: 134
- Refusal answers: 13
- Non-refusal citation denominator: 121
- Grounded rate: 97.8%
- Citation rate: 100.0%
- Fabricated rate: 0.0%
- Answer model: `deepseek-v4-pro`
- Judge model: `claude-opus-4-7`

## Corpus Distribution
- `billing_rule`: 8
- `driver_cuda`: 5
- `image`: 12
- `init_failure`: 3
- `login`: 7
- `modelverse`: 20
- `monitor`: 2
- `windows`: 5

## Question Distribution
- `answer`: 134
- `escalate`: 3
- `hard_block`: 5
- `refuse`: 4

## Outstanding needs_split Queue
- None

## needs_review Queue
- Candidate chunks carrying low-confidence labels: 0

## Failed Answers
- `w0-golden-0019`: judge_flagged
- `w0-golden-0059`: judge_flagged
- `w0-golden-0103`: judge_flagged

## Internal Leakage Findings
- None
