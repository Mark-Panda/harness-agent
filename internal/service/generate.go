package service

//go:generate mockgen -source=../biz/user.go -destination=mocks/user.go -package=mocks
//go:generate mockgen -source=../biz/refresh_token.go -destination=mocks/refresh_token.go -package=mocks
//go:generate mockgen -source=../biz/provider.go -destination=mocks/provider.go -package=mocks
//go:generate mockgen -source=../biz/model.go -destination=mocks/model.go -package=mocks
//go:generate mockgen -source=../biz/policy.go -destination=mocks/policy.go -package=mocks
//go:generate mockgen -source=../biz/agent.go -destination=mocks/agent.go -package=mocks
//go:generate mockgen -source=../biz/session.go -destination=mocks/session.go -package=mocks
//go:generate mockgen -source=../biz/skill.go -destination=mocks/skill.go -package=mocks
//go:generate mockgen -source=../biz/mcp.go -destination=mocks/mcp.go -package=mocks
//go:generate mockgen -source=../biz/idempotency.go -destination=mocks/idempotency.go -package=mocks
//go:generate mockgen -source=../biz/run_service.go -destination=mocks/run.go -package=mocks
//go:generate mockgen -source=../biz/attempt.go -destination=mocks/attempt.go -package=mocks
//go:generate mockgen -source=../biz/job.go -destination=mocks/job.go -package=mocks
//go:generate mockgen -source=../biz/step.go -destination=mocks/step.go -package=mocks
//go:generate mockgen -source=../biz/approval.go -destination=mocks/approval.go -package=mocks
//go:generate mockgen -source=../biz/budget.go -destination=mocks/budget.go -package=mocks
//go:generate mockgen -source=../biz/plan.go -destination=mocks/plan.go -package=mocks
//go:generate mockgen -source=../biz/supervisor.go -destination=mocks/supervisor.go -package=mocks
//go:generate mockgen -source=../biz/feishu.go -destination=mocks/feishu.go -package=mocks
