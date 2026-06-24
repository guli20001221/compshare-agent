# Community Image Targeted External KB Expansion - 2026-06-24

## Summary

I called the live read-only `DescribeCommunityImages` API with three popularity-oriented sorts:

- `CreatedCount`
- `ImageUseTime`
- `FavoritesCount`

The API snapshot is saved at:

- `docs/research/community_image_popular_snapshot_2026-06-24.json`

The top 20 results from the three sorts were identical in this snapshot. I used the image list as a demand signal only. Technical facts in the external KB were taken from upstream project documentation, not inferred from image titles.

This report records the community-image-targeted stage. The current corpus was later extended to 180 chunks; see `docs/research/external_kb_second_wave_audit_2026-06-24.md`, `docs/research/external_kb_session_637_targeted_audit_2026-06-24.md`, and `docs/research/external_kb_cuda_compile_cleanup_audit_2026-06-24.md`.

## Hot image themes found

The popular community images clustered around these customer-question areas:

- LTX-2 / LTX-Video and ComfyUI video workflows
- LiveTalking, InfiniteTalk, MultiTalk, and other digital-human workflows
- SVC, TTS, voice cloning, and video dubbing
- AI-Toolkit LoRA training for FLUX/Qwen/Wan
- Qwen-Image and Wan2.2 image/video generation
- ComfyUI-GGUF low-VRAM image generation
- TripoSplat single-image 3D Gaussian generation

## Upstream sources used

- LTX-Video: https://github.com/Lightricks/LTX-Video
- ComfyUI-LTXVideo: https://github.com/Lightricks/ComfyUI-LTXVideo
- LiveTalking: https://github.com/lipku/LiveTalking
- InfiniteTalk: https://github.com/MeiGen-AI/InfiniteTalk
- MultiTalk: https://github.com/MeiGen-AI/MultiTalk
- so-vits-svc: https://github.com/svc-develop-team/so-vits-svc
- Amphion: https://github.com/open-mmlab/Amphion
- dots.tts: https://github.com/rednote-hilab/dots.tts
- CosyVoice: https://github.com/FunAudioLLM/CosyVoice
- VoxCPM: https://github.com/OpenBMB/VoxCPM
- Seed-TTS-Eval: https://github.com/BytedanceSpeech/seed-tts-eval
- AI-Toolkit: https://github.com/ostris/ai-toolkit
- Qwen-Image: https://github.com/QwenLM/Qwen-Image
- Wan2.2: https://github.com/Wan-Video/Wan2.2
- ComfyUI-GGUF: https://github.com/city96/ComfyUI-GGUF
- TripoSplat: https://github.com/VAST-AI-Research/TripoSplat
- GPT-SoVITS: https://github.com/RVC-Boss/GPT-SoVITS
- F5-TTS: https://github.com/SWivid/F5-TTS

## Added chunks

13 external chunks were added:

- `ext-community-ltx-video-comfyui-001`
- `ext-community-digital-human-livetalking-001`
- `ext-community-digital-human-infinitetalk-001`
- `ext-community-voice-conversion-svc-001`
- `ext-community-dots-tts-voice-cloning-001`
- `ext-community-cosyvoice-voxcpm-tts-001`
- `ext-community-seed-tts-eval-001`
- `ext-community-ai-toolkit-lora-001`
- `ext-community-qwen-image-001`
- `ext-community-wan-video-001`
- `ext-community-comfyui-gguf-001`
- `ext-community-triposplat-3d-001`
- `ext-community-video-dubbing-tts-webui-001`

The external corpus increased from 120 to 133 chunks.

The golden retrieval set increased from 113 to 126 questions.

## Artifact pins after this stage

- External corpus digest: `ea6b9f9a92798c6a8d475b011b0539f91f28b133f5a58170a9d9e48538e8ef46`
- External qwen3 sidecar digest: `9625873990d16a9a2b2112beb918af7c8713d9abd7a9dae2c6ba7acb829ffe61`
- Sidecar file: `deploy/kb/embeddings_ea6b9f9a92798c6a8d475b011b0539f91f28b133f5a58170a9d9e48538e8ef46_qwen3-embedding-8b.jsonl`

These pins were superseded by the second-wave digest `b9457548a185ca1abbb6932b01c6472626f18c1ab38b39c30b327fbae4d48321`, then by the 637-session-targeted digest `16692137b3a4438a9e1e47412cc77fa04904bfcd26f40b43d6779c5bcc652e8f`, and then by the current cleanup/compile/toolkit digest `03d16590076cc8e4eee005962277281b896a595b62a5e9779c5f71dbad832a1c`.

## Verification

Fresh verification passed on 2026-06-24:

- `python scripts/rag_w0/validate_chunks.py --chunks deploy/kb/external_w0.jsonl`
- `python scripts/rag_w0/check_internal_leakage.py --chunks deploy/kb/external_w0.jsonl` -> 133 chunks, 0 flagged
- `python -m py_compile scripts/rag_ext/build_pilot_chunks.py scripts/rag_ext/build_external_golden.py scripts/rag_ext/run_external_retrieval_eval.py`
- `go test ./internal/knowledge -run "TestLoadExternalCorpusPinnedRealData|TestMergePlatformAndExternalRealData" -count=1`
- `go test ./cmd -run "TestExternalKnowledge|TestLoadKnowledgeCorporaMergeAndDegrade" -count=1`
- `python scripts/rag_ext/run_external_retrieval_eval.py --chunks deploy/kb/external_w0.jsonl --questions scripts/rag_ext/external_golden.jsonl --out eval/rag_ext_external_retrieval_2026-06-24.json --embeddings-path deploy/kb/embeddings_ea6b9f9a92798c6a8d475b011b0539f91f28b133f5a58170a9d9e48538e8ef46_qwen3-embedding-8b.jsonl --mode qwen3_rrf --env F:/compshare-agent/.env.local`

Retrieval result:

- Overall: 126/126 Top-3 hits
- Community image group: 13/13 Top-3 hits
- Gate: pass (`top_3_hit_rate = 1.0 >= 0.85`)
