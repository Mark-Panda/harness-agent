package biz

import "sync/atomic"

// inflightJobs 记录当前进程内在途的后台任务数。
var inflightJobs atomic.Int64

// workerDraining 标记进程进入停机排空阶段；置位后各 Worker 停止认领新任务。
var workerDraining atomic.Bool

// beginJob 标记一个后台任务开始，返回结束回调以递减在途计数；outcome 仅用于调用方保留语义。
func beginJob() func(outcome string) {
	inflightJobs.Add(1)
	return func(string) {
		inflightJobs.Add(-1)
	}
}

// InflightJobs 返回当前在途后台任务数，用于优雅停机等待任务收敛。
func InflightJobs() int64 {
	return inflightJobs.Load()
}

// BeginWorkerDrain 进入停机排空阶段：Worker 停止认领新任务，在途任务继续执行到完成或超时。
func BeginWorkerDrain() {
	workerDraining.Store(true)
}

// WorkerDraining 返回进程是否正在排空；Worker 主循环据此停止认领。
func WorkerDraining() bool {
	return workerDraining.Load()
}
