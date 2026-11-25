// Package workerpool 定义 Worker 资源池的 lane 常量与校验，供领域层和配置层共享。
package workerpool

// Lane 取值：Run 按 Agent 模式进入 react/plan/super，后台任务固定 background，飞书事件固定 feishu。
const (
	// LaneReact ReAct 执行池。
	LaneReact = "react"
	// LanePlan Plan-and-Execute 执行池。
	LanePlan = "plan"
	// LaneSupervisor Supervisor 执行池。
	LaneSupervisor = "super"
	// LaneBackground 摘要、向量化、事实提取、技能提炼等后台任务池。
	LaneBackground = "background"
	// LaneFeishu 飞书事件消费池。
	LaneFeishu = "feishu"
)

// IsRunLane 判断 lane 是否是可被执行 Worker 认领的 Run lane。
func IsRunLane(lane string) bool {
	switch lane {
	case LaneReact, LanePlan, LaneSupervisor:
		return true
	default:
		return false
	}
}
