# Linux-ops + PyTorch-basics external-corpus CLI smoke report

Merged runtime index (platform 687 + external 51, default-on), deepseek-v4-flash, read-only. 9/9 PASS.

One run; deepseek-v4-flash is non-deterministic, so per-row `intent`
(knowledge_qa vs diagnosis lane — both ground on the same evidence) and
the exact `anchors hit` vary between runs. The PASS criterion is robust to
that: a vertical row passes on `retrieved` (expected chunk in the turn's
retrieval trace) AND `anchors_any` (>=1 grounded token in the reply). The
`linux_ops-login-collision` row (ssh-免密) is the merged-index check: the
query infers product_area=login (no +2 boost toward the SSH-key chunk) yet
ext-linux-ssh-key-001 still retrieves + grounds. The control passes on no
Linux/PyTorch contamination of a platform-billing answer. Regenerate via
`pwsh -File eval/rag_ext_linuxpt_smoke.ps1 && python eval/rag_ext_linuxpt_smoke_judge.py`.

| qid | kind | verdict | intent | retrieved | cited | anchors hit |
|---|---|---|---|---|---|---|
| linux-tmux | linux_ops | PASS | knowledge_qa | True | True | tmux, attach |
| linux-disk | linux_ops | PASS | knowledge_qa | True | True | df, du |
| linux-conda | linux_ops | PASS | knowledge_qa | True | True | conda create, conda activate |
| linux-pip-mirror | linux_ops | PASS | knowledge_qa | True | True | index-url, -i |
| linux-ssh-key-collision | linux_ops-login-collision | PASS | knowledge_qa | True | True | ssh-keygen, authorized_keys |
| pytorch-install | pytorch_basics | PASS | diagnosis | True | False | is_available, nvidia-smi |
| pytorch-ddp | pytorch_basics | PASS | knowledge_qa | True | True | torchrun, DDP |
| pytorch-amp | pytorch_basics | PASS | knowledge_qa | True | True | autocast, GradScaler |
| ctrl-platform-billing | control-anticontam | PASS | knowledge_qa | False | False |  |
