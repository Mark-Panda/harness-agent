package biz

import "sync/atomic"

// inflightJobs 记录当前进程内在途的后台任务数。
var inflightJobs atomic.Int64

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
