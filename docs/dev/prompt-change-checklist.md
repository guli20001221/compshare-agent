# Prompt Change Checklist

Before changing planner or ReAct prompt text:

1. Decide whether the change is byte-stable or intentionally byte-changing.
2. For byte-stable refactors, run the prompt snapshot tests and keep SHA values unchanged.
3. For byte-changing edits, update the snapshot baseline in the same commit with a short reason.
4. Add or update a golden case covering intent, allowed/forbidden tools, runtime form, and boundary text.
5. Keep safety in code gates: tool whitelist, parameter validation, confirmation, and destructive-action refusal.
6. For rollout changes, compare baseline and candidate trace `outcome.prompt_tokens`; do not default on unless golden pass rate is unchanged and prompt tokens are reduced.
