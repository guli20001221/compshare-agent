# 路由准确率 eval 标签集（基于 637 全量，handling-class 标注）— 2026-06-22

> #2 产物。用 workflow `route-label-637`(20 并行 sonnet 标注器 + 1 独立盲标 recheck)给**全部 637 会话**按
> **expected handling-class**(下游处理分叉的 MECE 稳定主标签)标注,产出路由准确率 eval 的标签集。
> 这是评估 P0 阶段0「路由准确率门」的标签源,也用 LLM 标签**独立复核**了意图审计 §4 的 MECE 结论。
>
> 产物:
> - 全量标签:`eval/realism/route_label_637/labels_full.jsonl`(637 条)
> - 平衡 eval 子集:`eval/realism/route_label_637/routing_eval_set.jsonl`(173 条,每 handling-class ≤25)
> - 统计:`eval/realism/route_label_637/_agg_stats.json`;聚合脚本 `eval/realism/aggregate_route_labels.py`

## 1. 覆盖与标注可靠性
- **覆盖 637/637**(0 缺 / 0 重 / 0 越界)。
- **标注信度(独立盲标 40 分层样本)**:handling_class 一致率 **0.850**,**Cohen's κ = 0.790**(substantial→almost-perfect 边界);fine_intent 精确一致 **90%**。
- → 标签质量足以作 **silver labels**(路由 eval 基线);若要升 gold,人工抽检 ~40 条即可(κ 已证一致性高,人工只需校 6 个分歧 case)。
- 6 个 handling_class 分歧**全落在非 MECE 接缝上**(refuse_out_of_scope↔knowledge_answer 4 个=部分可覆盖话题、knowledge↔diagnosis 1、ambiguous↔refuse 1)——分歧本身就是 MECE 债的指纹。

## 2. Handling-class 分布(权威 LLM 标签,读全会话)
| handling_class | 数量 | 占比 | 下游处理 |
|---|---:|---:|---|
| read_query | 265 | 41.6% | 只读 API |
| knowledge_answer | 159 | 25.0% | KB 检索回答 |
| lifecycle_mutate | 72 | 11.3% | 确认门 saga |
| diagnosis | 48 | 7.5% | 诊断流程 |
| refuse_out_of_scope | 45 | 7.1% | 诚实拒答 |
| greeting_smalltalk | 19 | 3.0% | 问候 |
| create_deploy | 16 | 2.5% | 创建确认 saga |
| ambiguous | 13 | 2.0% | 澄清 |

**与审计 §3 关键字首过对账(互相印证)**:LLM 的 read_query 41.6% ≈ 关键字 stock 22.6%+resource_info 19.6%=42.2%(读类合一);LLM 的 knowledge_answer 25.0% ≈ 关键字 knowledge_qa 9.1% + 大半 UNMAPPED 16.3%——**即我那 16.3% catch-all 里大部分其实是"可答 how-to",被全会话标注还原成 knowledge_answer;真·域外只有 ~11%**(refuse 7.1% + 部分 ambiguous/none)。比关键字首过更准。

## 3. MECE 复核(LLM 标签,比审计 §4 更权威)
- **multi_handling(真跨 >1 handling-class)= 132 / 637 = 20.7%**(读全多轮会话得到,远高于审计关键字首过的 6.9%)。**每 5 个会话就有 1 个跨处理边界** → 单标签路由是有损的。
- **`mixed_*` 逃生舱覆盖率**:fine_intent 里 mixed_diagnosis_kb 20 + mixed_billing_kb 12 = 32 条,仅覆盖 132 个多处理会话的 **24%** → **印证审计 §4a「mixed 逃生舱只覆盖一小部分真实重叠 = 非完备」**,且更量化。
- **image/model 6 件套 fine_intent 合计 = 8 / 637 = 1.3%** → **印证审计 §4b「6 个标签服务 <3% 流量 = 过度切分」**(LLM 标签甚至更低)。
- **长尾确认**:disk_info 0.2%(1)、monitor_query 0.3%(2)、network_accelerator_status 0.3%(2)、gpu_specs_query 1.1%(7)、pricing_query 1.3%(8)。
- **fine_intent "none" = 8.6%(55)**:无法映射到 26 标签之一(多为 refuse/greeting/ambiguous)= 覆盖缺口。
- **axisB**:B0_na 428(无需评文本)/ B1_judge 161(能力内待 judge)/ B2_refusal 48(域外拒答)。→ 接 #3 judge+kappa 的内容评测面 = 161+48。

## 4. 平衡 eval 子集(routing_eval_set.jsonl,173 条)
按 handling-class 分层、每类 ≤25(低频类全收)、优先高置信:read_query 25 / lifecycle_mutate 25 / knowledge_answer 25 / diagnosis 25 / refuse_out_of_scope 25 / greeting 19 / create_deploy 16 / ambiguous 13。
- **为什么平衡**:真实分布 read_query 占 42%,直接按频率会让分类器"全猜 read_query"也拿高分;平衡子集让每类都有足够样本测**该类的**路由准确率(尤其 create_deploy/diagnosis/refuse 这些高 misroute 代价的少数类)。
- **真实频率分布**(labels_full.jsonl)单独保留,用于报告加权准确率 + 反映真实负载。

## 5. 怎么用(与去耦原则的关系)
- 这是**有意按意图/handling 标签耦合的分类器评测**(与 #1 轴A 行为门**故意相反**:行为门只断言可观测结果、改路由不动;路由 eval 主动按标签评、改 taxonomy 时**主动重标小集=维护非冲突**)。
- **建议主评 handling_class(8 类),次评 fine_intent(26)**:handling_class 是 MECE 稳定轴,审计 §5 的收敛(image 6→1、长尾合并)**不改 handling_class 标签**→ 这套 eval 收 taxonomy 时基本不用重标,只有 fine_intent 列需跟着 remap。
- **✅ 真模型 eval 臂已就位(2026-06-22,3 文件 +119 行,go test ./eval/... 全绿)**:`eval/models.go` 加 `deepseek-v4-flash` 条目;`eval/evaluate_test.go::TestEval` 加 flag-driven 阈值门(`-min-intent-acc/-min-tool-acc/-min-content-acc`,默认 0=report-only)+ no-key 守卫;`eval/intent/offline_eval_test.go` 加 `-model` 真路由 `TestOnlineRouterEval`。运行:`go test ./eval/intent/ -run TestOnlineRouterEval -model deepseek-v4-flash`(先无阈值测基线,再 `-min-intent-acc <floor>` 设门),需 `LLM_API_KEY`。
- **本 routing_eval_set(handling_class schema)的专用 eval = 待加**:`TestOnlineRouterEval` 现跑 `fixtures.jsonl`(intent schema);routing_eval_set 是 handling_class schema,需新 test 适配——用真路由器对 173 跑分类、对 handling_class 算 per-class 准确率/混淆矩阵、设阈值门。

## 6. 对审计 §5 收敛建议的数据支撑(本节=用 637 真频率给建议排序)
| 审计 §5 动作 | 本数据支撑 | 优先级 |
|---|---|---|
| image 6→1 + facet | 6 件套仅 1.3% 流量、各 ≤0.5% | 高(降复杂度,几乎无频率损失) |
| 合并长尾(monitor query/history、expiry→billing、vague_failure→unknown) | monitor 0.3%、disk_info 0.2%、vague_failure 2.8%、net_accel 0.3% | 中 |
| 补碰撞逃生舱 / how-to 归 knowledge_qa | multi_handling 20.7%、mixed 仅覆盖 24% | **高**(最大 misroute 源) |
| 域外出口标准化(轴B B2) | refuse 7.1% + ambiguous 2.0% = 真域外 ~9-11% | 高(②已就位) |

## 7. 局限
- 标签 = sonnet 标 + opus 盲标复核(κ 0.79);**expected handling 是判断**,非客服 gold。升 gold 需人工抽检(建议先校 6 个分歧 case + 随机 20)。
- handling_class 按"应该怎么处理"标,与当前 main 实际路由的差距 = 路由 eval 真正要测的(本集是 ground truth 侧,模型输出侧由阶段0 真模型臂产生)。
- 平衡 eval 子集用于 per-class 准确率;真实负载加权另算。
