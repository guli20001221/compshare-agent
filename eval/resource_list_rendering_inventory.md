# 资源查询类列表渲染盘点 (③) + clean-display fix

## Inventory — which deterministic list renders cleanly vs "像镜像一样比较乱"
| 列表 | renderer (internal/intent) | 标签 | 裸 ID? | 裁剪/上限 | 判定 |
|---|---|---|---|---|---|
| GPU 规格 | `buildGPUSpecLines` | 中文 (机型/性能/显存/状态) | 否 | 去重 by name | **clean** |
| 库存 stock | `renderStockReply` | 中文 (机型/状态) | 否 | 去重 | **clean** |
| 我的实例 | `RenderResourceSummary` | 中文 (resourceLabel*) | UHostId（已标注） | 截断 N/M 台 | **mostly clean** |
| 社区镜像组头 | `buildCommunityGroupHeader` | 中文 (名称/作者/版本数) | 否 | group limit | **clean** |
| **平台/自制镜像列表** | `renderImageListReply` | **英文 Key=Value** | **CompShareImageId** | **无（全 39）** | **MESSY** ← 用户投诉 |
| **社区镜像版本行** | `buildCommunityVersionLine` | **英文 Key=Value** | **CompShareImageId 居首** | 每组上限 | **MESSY** |
| **共享镜像列表** | `buildSharedImageLine` | **英文 Key=Value** | **CompShareImageId 居首** | cap 20 | **MESSY** |

**结论**：GPU/库存/实例/社区组头本就干净（中文标签、去重、无裸 ID）；**只有镜像家族**（平台/自制/社区版本/共享）是英文 `Key=Value` 倾倒、裸 `CompShareImageId` 居首、无裁剪——正是「不要像镜像一样输出来的比较乱」。

## Fix — handler 层确定性裁剪显示载荷
- `renderImageListReply`（verbatim / `fast_template` 路径）：改为 **名称-first + 中文标签**（`imageFieldLabel`）+ **去掉裸 CompShareImageId**（`imageDisplaySkipFields`）+ **上限 `imageListDisplayCap=30`** + 溢出提示「共 N 个，已显示前 30 个；可补充关键词筛选」。
- `buildImageListEnvelope`（grounded 路径）：丢弃 CompShareImageId / 冗余 name 的 display Fact（id 仍在 `Subject.ID`、name 在 `Subject.Name`）+ subjects 截断到 30 + `display_truncated` Computed 提示。
- `buildSharedImageLine` / `buildCommunityVersionLine`：名称-first、去裸 ID、中文标签。

## Live CLI smoke (merged runtime, USE_GROUNDED_RENDERER=llm)
「列出全部镜像」→ 按 System/App/Other **分组的整洁 markdown 表**，**截断到 30/39 + 溢出提示**，无冗余 name 列。对比改前的「`Name=X, CompShareImageId=img-xxx, ImageType=App` 单行倾倒 × 39」——「乱」的问题解决。

## Grounded 渲染器收口
默认 grounded 渲染器原先要求镜像列表必须展示 `CompShareImageId`，这会把确定性路径已经清理掉的裸 ID 又带回来。现已同步收口：`image_list` 默认只展示镜像名称和人可读字段，只有用户明确询问 ID 时才展示镜像 ID；实例列表仍保留实例 ID 规则，不影响用户按 UHostId 定位实例。
