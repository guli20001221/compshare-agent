# PR2 router A/B report

## Summary

The PR2 prompt-slimming A/B requires a live router model. It was not executed in this local shell because no model/API environment variables were present.

## Required setup

- Model: deepseek-v4-flash
- Runs: N >= 5 per case
- A arm: pre-PR2 planner schema/prompt baseline
- B arm: latest main with router-output slimming

## Required case groups

- Create: 为我创一台V100S的实例; 帮我创建一台4090; 在华北二A创建一台4090
- Deploy: 部署 DeepSeek R1 32B; 部署一台4090跑Qwen
- Price: 4090多少钱; 4090折后价是多少
- Knowledge: DeepSeek R1怎么部署; 系统镜像和基础镜像有什么区别
- Image browse: 我自己保存的镜像有哪些; 有哪些 PyTorch 镜像
- Lifecycle/resize: 明确目标开机、关机、重启、改配样本

## Acceptance

- No new schema-invalid retries.
- Create/deploy/read-only boundaries do not regress.
- Price, advice, comparison, and how-to questions do not open paid confirmation cards.
- Lifecycle and resize requests do not fall into knowledge or create routes.

Status: blocked locally until a live model key is available.

