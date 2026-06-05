# Prompt Golden Cases

This directory pins prompt-sensitive behavior for ReAct prompt cardization.

- `cases.json` is schema-checked by `go test ./internal/prompt`.
- Live CLI checks use `eval/context_prompt_cli_regression.ps1 -CasesPath eval/prompt_goldens/cases.json`.
- Rollout comparison uses `run_prompt_rollout.ps1`, which runs baseline and `USE_INTENT_SCOPED_REACT_PROMPT=1` against the same cases and compares trace `outcome.prompt_tokens`.

Live scripts require `LLM_API_KEY` plus normal smoke credentials. Unit checks do not.
