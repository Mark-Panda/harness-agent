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

## 变更规则

- 修改模型、Prompt、Skill、MCP、Embedding 或预算阈值时，必须更新本基线并重新执行 `make test-quality`。
- 结果低于基线即视为质量回退；只有书面风险接受与人工评审可以临时放行。
- 实时模型验收在预发布环境执行，固定模型/参数/Prompt 快照并重复运行，结果与基线比较后归档。
