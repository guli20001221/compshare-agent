# Prompt Golden Cases

This directory pins construction properties of the single central-Agent prompt.

- `cases.json` is schema-checked by `go test ./internal/prompt`.
- Live CLI checks use `eval/context_prompt_cli_regression.ps1 -CasesPath eval/prompt_goldens/cases.json`.
- There is no alternate intent-scoped prompt arm. Runtime acceptance uses the real-context replay suite described in the convergence plan.

Live scripts require `LLM_API_KEY` plus normal smoke credentials. Unit checks do not.
