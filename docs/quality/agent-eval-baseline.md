# Agent 黄金集与对抗集基线

版本：`testdata/golden.json` v1，基线日期 2026-11-03。执行：`make test-quality`。

## 阈值

| 指标 | 基线 | 说明 |
| --- | --- | --- |
| 单 Run 最大模型步数 | 12 | 超过即 `ErrStepLimit` |
| 连续重复工具调用阈值 | 3 | 达到即 `ErrToolLoop` |
| 单工具结果进入上下文上限 | 64 KiB | 超过替换为 `tool_result_too_large` 错误标记 |
| 跨用户记忆召回 | 0 条 | 任何跨用户召回均为阻断缺陷 |
| 注入内容进入 system 角色 | 0 次 | 工具结果只能作为不可信 tool 消息 |
| 黄金集通过率 | 100% | 固定脚本模型，结果必须确定性一致 |

## 场景覆盖

| 能力 | 场景/测试 | 断言 |
| --- | --- | --- |
| ReAct 工具调用与最终回答 | `react_tool_success` | 工具调用 1 次、最终回答与基线一致 |
| Prompt Injection 防护 | `react_prompt_injection_tool_result` | 注入文本只出现在 tool 消息，不进入 system |
| 循环终止 | `adversarial_tool_loop` | 返回 `ErrToolLoop` |
| 未知工具 | `adversarial_unknown_tool` | 可恢复错误标记，不执行任意工具 |
| 超大工具结果 | `adversarial_oversize_result` | 原文不进入上下文 |
| Plan 成功/重试/恢复/取消/成本 | `go test ./internal/biz -run 'TestPlan'` | 状态机与预算断言 |
| Supervisor 部分失败/并发/取消 | `go test ./internal/biz -run 'TestSupervisor'` | 部分失败显式、子任务不孤儿 |
| 记忆相关性/冲突/摘要忠实度 | `REQUIRE_DATABASE=true go test -tags=integration ./internal/data -run TestMemoryQualityEvaluation` | Precision/Recall、MMR、事实冲突、跨用户零泄漏 |
| 删除后不可召回 | `REQUIRE_DATABASE=true go test -tags=integration ./internal/data -run 'TestDeletion|TestPurgeExpired'` | 向量与摘要清理 |

## 记忆质量评测 v1

数据集：`tests/quality/memory/testdata/dataset.v1.json`；门禁：`baseline.v1.json`；执行：`make test-memory-eval`（必须使用独立空库）。

| 指标 | 基线 | 说明 |
| --- | --- | --- |
| `recall@8` | ≥ 0.90 | 相关记忆召回覆盖率 |
| `precision@8` | ≥ 0.50 | 小规模确定性数据集的下限 |
| `MRR` | ≥ 0.80 | 首个相关记忆排名 |
| `SummaryFaithfulness` | ≥ 0.80 | 摘要 token 被来源覆盖比例 |
| `SummaryClaimRecall` | ≥ 0.90 | 期望 claim 出现在摘要中的比例 |
| `UnsupportedClaimRate` | 0 | 摘要不得出现禁止 claim |
| `SpeakerAttributionAccuracy` | 1.0 | 群聊 claim 必须归到正确发言人 |
| `ConflictResolutionAccuracy` | 1.0 | 用户纠正后生效事实必须为新值 |
| `CrossUserLeak` | 0 | 跨用户记忆召回为阻断缺陷 |
| `StalePreferenceLeak` | 0 | 已纠正旧偏好不得继续生效 |
| `DeletedMemoryLeak` | 0 | 删除后向量、摘要、事实、消息均不可召回 |

场景覆盖：中文/英文/日语/韩语检索、群聊发言人归属、长周期偏好冲突、摘要 claim 忠实度、会话删除后不可召回。

## 变更规则

- 修改模型、Prompt、Skill、MCP、Embedding 或预算阈值时，必须更新本基线并重新执行 `make test-quality`。
- 结果低于基线即视为质量回退；只有书面风险接受与人工评审可以临时放行。
- 实时模型验收在预发布环境执行，固定模型/参数/Prompt 快照并重复运行，结果与基线比较后归档。
