# External KB 637-Session-Targeted Expansion - 2026-06-24

## Summary

This update adds external support knowledge guided by the 637 real-session routing analysis. The session data is used only as a demand signal; factual content is grounded in upstream official documentation, project READMEs, and Linux/NVIDIA manuals.

The expansion targets frequent customer questions around SD-WebUI/A1111, ControlNet, WebUI refused-connection triage, SSH and transfer failures, Ollama model/cache/context issues, Open WebUI/LiteLLM, Docker GPU visibility, card-count mismatch, NVIDIA MPS, DVC/object-storage workflows, Label Studio, persistence boundaries, and background service patterns.

This report records the 637-session-targeted stage. The current corpus was later extended to 180 chunks in `docs/research/external_kb_cuda_compile_cleanup_audit_2026-06-24.md`.

## Added chunks

18 external chunks were added:

- `ext-a1111-webui-listen-port-001`
- `ext-a1111-model-paths-001`
- `ext-sd-webui-controlnet-install-001`
- `ext-webui-refused-connection-debug-001`
- `ext-ssh-connection-debug-001`
- `ext-scp-rsync-transfer-debug-001`
- `ext-ollama-model-cache-repair-001`
- `ext-ollama-context-vram-001`
- `ext-openwebui-ollama-openai-001`
- `ext-litellm-proxy-openai-compatible-001`
- `ext-docker-gpu-visible-devices-advanced-001`
- `ext-gpu-card-count-mismatch-001`
- `ext-nvidia-mps-single-node-sharing-001`
- `ext-dvc-data-versioning-001`
- `ext-s5cmd-minio-object-transfer-001`
- `ext-label-studio-dataset-annotation-001`
- `ext-model-files-persistence-boundary-001`
- `ext-systemd-user-service-webui-001`

The external corpus increased from 158 to 176 chunks.

The golden retrieval set increased from 151 to 169 questions.

## Session signals used

The 637-session routing report showed that `knowledge_answer` represented 159/637 sessions, and the committed 173-case balanced subset contains repeated support questions around:

- SD-WebUI / ComfyUI / Jupyter pages not reachable.
- SSH login and file transfer failures.
- GPU not detected, card count mismatch, and no-device errors.
- Ollama model launch failures and model-cache confusion.
- Data persistence and storage boundary questions.

These were translated into platform-neutral external troubleshooting chunks.

## Upstream sources used

- AUTOMATIC1111 Stable Diffusion WebUI: https://github.com/AUTOMATIC1111/stable-diffusion-webui
- A1111 command-line arguments: https://github.com/AUTOMATIC1111/stable-diffusion-webui/wiki/Command-Line-Arguments-and-Settings
- sd-webui-controlnet: https://github.com/Mikubill/sd-webui-controlnet
- Open WebUI quick start: https://docs.openwebui.com/getting-started/quick-start/
- Open WebUI README: https://github.com/open-webui/open-webui
- LiteLLM getting started: https://docs.litellm.ai/docs/
- LiteLLM OpenAI-compatible endpoints: https://docs.litellm.ai/docs/providers/openai_compatible
- Ollama FAQ: https://docs.ollama.com/faq
- Ollama CLI: https://docs.ollama.com/cli
- Ollama context length: https://docs.ollama.com/context-length
- Linux `ss` manual: https://man7.org/linux/man-pages/man8/ss.8.html
- curl manual: https://curl.se/docs/manpage.html
- OpenSSH config and scp manuals.
- rsync manual: https://download.samba.org/pub/rsync/rsync.1
- NVIDIA Container Toolkit Docker configuration: https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/docker-specialized.html
- NVIDIA MPS: https://docs.nvidia.com/deploy/mps/index.html
- DVC get started and remote storage: https://doc.dvc.org/start
- s5cmd: https://github.com/peak/s5cmd
- MinIO mc: https://github.com/minio/mc
- MinIO mc mirror: https://docs.min.io/aistor/reference/cli/mc-mirror/
- Label Studio README: https://github.com/HumanSignal/label-studio
- Label Studio guide: https://labelstud.io/guide/

## Artifact pins after this stage

- External corpus digest: `16692137b3a4438a9e1e47412cc77fa04904bfcd26f40b43d6779c5bcc652e8f`
- External qwen3 sidecar digest: `16874f76d0f7e6f5f8fb4de99f3df83612c6eaa260c8943a60168d2dd44e0979`
- Sidecar file: `deploy/kb/embeddings_16692137b3a4438a9e1e47412cc77fa04904bfcd26f40b43d6779c5bcc652e8f_qwen3-embedding-8b.jsonl`
- Retrieval result file: `eval/rag_ext_external_retrieval_2026-06-24.json`

These pins were superseded by the cleanup/compile/toolkit digest `03d16590076cc8e4eee005962277281b896a595b62a5e9779c5f71dbad832a1c` and sidecar digest `841a209b522144612010ee9e92ba8b53b90b6c556a939cdcc20e742f4fe46d7d`.

## Verification

Fresh verification passed on 2026-06-24:

- `python scripts/rag_w0/validate_chunks.py --chunks deploy/kb/external_w0.jsonl`
- `python scripts/rag_w0/check_internal_leakage.py --chunks deploy/kb/external_w0.jsonl` -> 176 chunks, 0 flagged
- `python -m py_compile scripts/rag_ext/build_pilot_chunks.py scripts/rag_ext/build_external_golden.py scripts/rag_ext/run_external_retrieval_eval.py`
- `go test ./internal/knowledge -run "TestLoadExternalCorpusPinnedRealData|TestMergePlatformAndExternalRealData" -count=1`
- `go test ./cmd -run "TestExternalKnowledge|TestLoadKnowledgeCorporaMergeAndDegrade" -count=1`
- `python scripts/rag_ext/run_external_retrieval_eval.py --chunks deploy/kb/external_w0.jsonl --questions scripts/rag_ext/external_golden.jsonl --out eval/rag_ext_external_retrieval_2026-06-24.json --embeddings-path deploy/kb/embeddings_16692137b3a4438a9e1e47412cc77fa04904bfcd26f40b43d6779c5bcc652e8f_qwen3-embedding-8b.jsonl --mode qwen3_rrf --env F:\compshare-agent\.env.local`

Retrieval result:

- Overall: 169/169 Top-3 hits
- 637-session-targeted group: 18/18 Top-3 hits
- Gate: pass (`top_3_hit_rate = 1.0 >= 0.85`)
