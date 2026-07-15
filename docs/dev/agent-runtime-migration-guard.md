# Agent Runtime 迁移门禁

本门禁服务于 `2026-07-15-agent-runtime-convergence-spec.md`。它不判断现有规则是否正确，只阻止迁移期间继续增加同类补丁。

## 日常检查

```powershell
go run ./cmd/architecture-audit
go test ./internal/architectureguard -count=1
```

检查内容：

- 生产 Go 代码中的正则与 `Contains/HasPrefix/HasSuffix` 调用；
- 名称表明其承担自然语言动作、槽位、上下文或直接分派职责的符号；
- 当前模型前业务终止出口和语义作者源是否仍与迁移清单一致。

基线是“允许存在的最大集合”：删除旧规则不需要更新基线；新增规则会失败。不要为了通过 CI 直接运行 `-write`。只有确认新增点属于协议、安全、严格实体格式或其他非业务语义规则，并完成代码评审后，才允许更新基线。

生成审计候选：

```powershell
go run ./cmd/architecture-audit -write
```

`docs/audits/agent-runtime-migration-inventory.json` 中的条目必须随着迁移删除或改变目标，不允许用改名规避检查。每个行为切换 PR 都应同步删除已经退出生产路径的清单项。
