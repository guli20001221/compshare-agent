# ComfyUI external-corpus CLI smoke report

Merged runtime index (platform 687 + external 36, default-on), deepseek-v4-flash, read-only. 8/8 PASS.

One run; deepseek-v4-flash is non-deterministic, so per-row `intent`
(knowledge_qa vs diagnosis lane — both ground on the same evidence) and
the exact `anchors hit` vary between runs. The PASS criterion is robust to
that: a ComfyUI row passes on `retrieved`(expected chunk in the turn's
retrieval trace) AND `anchors_any` (>=1 grounded token in the reply); the
control passes on no-ComfyUI-contamination. Regenerate via
`pwsh -File eval/rag_ext_comfyui_smoke.ps1 && python eval/rag_ext_comfyui_smoke_judge.py`.

| qid | kind | verdict | intent | retrieved | cited | anchors hit |
|---|---|---|---|---|---|---|
| comfy-start | comfyui | PASS | knowledge_qa | True | True | --listen, 8188 |
| comfy-oom | comfyui | PASS | knowledge_qa | True | True | --lowvram, --novram |
| comfy-models-dir | comfyui-modelverse-collision | PASS | knowledge_qa | True | True | checkpoints |
| comfy-custom-nodes | comfyui | PASS | knowledge_qa | True | True | custom_nodes, git clone |
| comfy-install | comfyui | PASS | knowledge_qa | True | True | git clone, requirements.txt |
| comfy-cant-connect | comfyui | PASS | knowledge_qa | True | True | --listen, 8188 |
| comfy-api | comfyui | PASS | knowledge_qa | True | True | /prompt, API |
| ctrl-platform-api | control-anticontam | PASS | knowledge_qa | False | False |  |
