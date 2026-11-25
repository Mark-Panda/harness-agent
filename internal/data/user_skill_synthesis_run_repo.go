package data

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// userSkillSynthesisRunModel 对应 user_skill_synthesis_runs 表：每日提炼执行记录。
type userSkillSynthesisRunModel struct {
	// ID 记录主键。
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// UserID 所属用户，RLS 隔离键。
	UserID uuid.UUID `gorm:"type:uuid"`
	// RunDate 任务对应 UTC 日期。
	RunDate time.Time `gorm:"type:date"`
	// JobID 对应提炼任务。
	JobID *uuid.UUID `gorm:"type:uuid"`
	// Status 执行状态。
	Status string
	// Outcome 结果原因。
	Outcome string
	// Detail 脱敏执行说明。
	Detail string
	// SessionsScanned 扫描会话数。
	SessionsScanned int
	// SessionsUsed 参与提炼会话数。
	SessionsUsed int
	// MessagesUsed 参与提炼消息数。
	MessagesUsed int
	// CandidateName 候选技能名。
	CandidateName string
	// Confidence 置信度。
	Confidence float64
	// SkillID 生成的技能 ID。
	SkillID *uuid.UUID `gorm:"type:uuid"`
	// ResultsJSON 按助手聚合的结构化结果。
	ResultsJSON datatypes.JSON `gorm:"column:results_json"`
	// Attempts 当天累计执行次数。
	Attempts int
	// StartedAt 本次开始时间。
	StartedAt time.Time
	// FinishedAt 结束时间。
	FinishedAt *time.Time
	// UpdatedAt 最后更新时间。
	UpdatedAt time.Time
}

// TableName 指定提炼记录表名。
func (userSkillSynthesisRunModel) TableName() string { return "user_skill_synthesis_runs" }

// StartSkillSynthesisRun 开启或重启当天记录；同一用户同一天只保留一条并累加执行次数。
func (r *UserSkillRepository) StartSkillSynthesisRun(ctx context.Context, run biz.SkillSynthesisRun) (biz.SkillSynthesisRun, error) {
	if run.UserID == uuid.Nil {
		return biz.SkillSynthesisRun{}, fmt.Errorf("%w: skill synthesis run requires user id", biz.ErrInvalidConfig)
	}
	runDate, err := time.Parse("2006-01-02", strings.TrimSpace(run.Date))
	if err != nil {
		return biz.SkillSynthesisRun{}, fmt.Errorf("%w: invalid skill synthesis run date", biz.ErrInvalidConfig)
	}
	startedAt := run.StartedAt.UTC()
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	var record userSkillSynthesisRunModel
	err = WithScope(ctx, r.db, Scope{System: true, UserID: run.UserID}, func(tx *gorm.DB) error {
		return tx.Raw(`
INSERT INTO user_skill_synthesis_runs (id, user_id, run_date, job_id, status, started_at, updated_at, attempts)
VALUES (?, ?, ?, ?, 'running', ?, ?, 1)
ON CONFLICT (user_id, run_date) DO UPDATE SET
  job_id = EXCLUDED.job_id,
  status = 'running',
  outcome = '',
  detail = '',
  sessions_scanned = 0,
  sessions_used = 0,
  messages_used = 0,
  candidate_name = '',
  confidence = 0,
  skill_id = NULL,
  results_json = '[]'::jsonb,
  finished_at = NULL,
  started_at = EXCLUDED.started_at,
  updated_at = EXCLUDED.updated_at,
  attempts = user_skill_synthesis_runs.attempts + 1
RETURNING *`, uuid.New(), run.UserID, runDate, run.JobID, startedAt, startedAt).Scan(&record).Error
	})
	if err != nil {
		return biz.SkillSynthesisRun{}, catalogError("start skill synthesis run", err)
	}
	return toBizSkillSynthesisRun(record), nil
}

// FinishSkillSynthesisRun 写入当天记录的最终结果；记录不存在时返回未找到。
func (r *UserSkillRepository) FinishSkillSynthesisRun(ctx context.Context, userID, runID uuid.UUID, update biz.SkillSynthesisRunUpdate) (biz.SkillSynthesisRun, error) {
	if userID == uuid.Nil || runID == uuid.Nil {
		return biz.SkillSynthesisRun{}, fmt.Errorf("%w: skill synthesis run requires user and run ids", biz.ErrInvalidConfig)
	}
	finishedAt := update.FinishedAt.UTC()
	if finishedAt.IsZero() {
		finishedAt = time.Now().UTC()
	}
	results := update.Results
	if results == nil {
		results = []biz.SkillSynthesisRunResult{}
	}
	resultsJSON, err := json.Marshal(results)
	if err != nil {
		return biz.SkillSynthesisRun{}, err
	}
	var record userSkillSynthesisRunModel
	err = WithScope(ctx, r.db, Scope{System: true, UserID: userID}, func(tx *gorm.DB) error {
		return tx.Raw(`
UPDATE user_skill_synthesis_runs
SET status = ?, outcome = ?, detail = ?, sessions_scanned = ?, sessions_used = ?, messages_used = ?,
    candidate_name = ?, confidence = ?, skill_id = ?, results_json = ?, finished_at = ?, updated_at = ?
WHERE id = ? AND user_id = ?
RETURNING *`,
			string(update.Status), update.Outcome, truncateSkillRunDetail(update.Detail),
			update.SessionsScanned, update.SessionsUsed, update.MessagesUsed,
			update.CandidateName, update.Confidence, update.SkillID, datatypes.JSON(resultsJSON), finishedAt, finishedAt,
			runID, userID).Scan(&record).Error
	})
	if err != nil {
		return biz.SkillSynthesisRun{}, catalogError("finish skill synthesis run", err)
	}
	if record.ID == uuid.Nil {
		return biz.SkillSynthesisRun{}, fmt.Errorf("finish skill synthesis run: %w", biz.ErrResourceNotFound)
	}
	return toBizSkillSynthesisRun(record), nil
}

// ListSkillSynthesisRuns 按日期倒序列出用户最近的提炼记录。
func (r *UserSkillRepository) ListSkillSynthesisRuns(ctx context.Context, userID uuid.UUID, limit int) ([]biz.SkillSynthesisRun, error) {
	if userID == uuid.Nil {
		return nil, fmt.Errorf("%w: list skill synthesis runs requires user id", biz.ErrInvalidConfig)
	}
	if limit <= 0 || limit > 90 {
		limit = 30
	}
	var records []userSkillSynthesisRunModel
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Where("user_id = ?", userID).Order("run_date DESC").Limit(limit).Find(&records).Error
	})
	if err != nil {
		return nil, catalogError("list skill synthesis runs", err)
	}
	items := make([]biz.SkillSynthesisRun, 0, len(records))
	for _, record := range records {
		items = append(items, toBizSkillSynthesisRun(record))
	}
	return items, nil
}

// toBizSkillSynthesisRun 把数据库行转换为领域记录。
func toBizSkillSynthesisRun(record userSkillSynthesisRunModel) biz.SkillSynthesisRun {
	var results []biz.SkillSynthesisRunResult
	if len(record.ResultsJSON) > 0 {
		_ = json.Unmarshal(record.ResultsJSON, &results)
	}
	if results == nil {
		results = []biz.SkillSynthesisRunResult{}
	}
	return biz.SkillSynthesisRun{
		ID: record.ID, UserID: record.UserID, Date: record.RunDate.Format("2006-01-02"),
		JobID: record.JobID, Status: biz.SkillSynthesisRunStatus(record.Status),
		Outcome: record.Outcome, Detail: record.Detail,
		SessionsScanned: record.SessionsScanned, SessionsUsed: record.SessionsUsed,
		MessagesUsed: record.MessagesUsed, CandidateName: record.CandidateName,
		Confidence: record.Confidence, SkillID: record.SkillID, Results: results, Attempts: record.Attempts,
		StartedAt: record.StartedAt, FinishedAt: record.FinishedAt, UpdatedAt: record.UpdatedAt,
	}
}

// truncateSkillRunDetail 限制执行说明长度，避免异常堆栈或长错误污染记录。
func truncateSkillRunDetail(detail string) string {
	const maximum = 512
	if len(detail) <= maximum {
		return detail
	}
	return strings.ToValidUTF8(detail[:maximum], "")
}

// GetSkillSynthesisWatermark 返回最近一次成功分析到的消息时间；未初始化时 found=false。
func (r *UserSkillRepository) GetSkillSynthesisWatermark(ctx context.Context, userID uuid.UUID) (time.Time, bool, error) {
	if userID == uuid.Nil {
		return time.Time{}, false, nil
	}
	var coveredAt time.Time
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Raw(`SELECT covered_at FROM user_skill_synthesis_state WHERE user_id = ?`, userID).Scan(&coveredAt).Error
	})
	if err != nil {
		return time.Time{}, false, catalogError("get skill synthesis watermark", err)
	}
	if coveredAt.IsZero() {
		return time.Time{}, false, nil
	}
	return coveredAt.UTC(), true, nil
}

// AdvanceSkillSynthesisWatermark 只向前推进覆盖水位，避免并发乱序回退。
func (r *UserSkillRepository) AdvanceSkillSynthesisWatermark(ctx context.Context, userID uuid.UUID, coveredAt time.Time) error {
	if userID == uuid.Nil || coveredAt.IsZero() {
		return fmt.Errorf("%w: skill synthesis watermark requires user and time", biz.ErrInvalidConfig)
	}
	err := WithScope(ctx, r.db, Scope{System: true, UserID: userID}, func(tx *gorm.DB) error {
		return tx.Exec(`
INSERT INTO user_skill_synthesis_state (user_id, covered_at, updated_at)
VALUES (?, ?, now())
ON CONFLICT (user_id) DO UPDATE SET
  covered_at = GREATEST(user_skill_synthesis_state.covered_at, EXCLUDED.covered_at),
  updated_at = now()`, userID, coveredAt.UTC()).Error
	})
	if err != nil {
		return catalogError("advance skill synthesis watermark", err)
	}
	return nil
}

// CountAutoUserSkillsSince 统计指定时间后自动生成的用户技能数，用于每日额度控制。
func (r *UserSkillRepository) CountAutoUserSkillsSince(ctx context.Context, userID uuid.UUID, since time.Time) (int, error) {
	if userID == uuid.Nil || since.IsZero() {
		return 0, nil
	}
	var count int64
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Model(&userSkillModel{}).
			Where("user_id = ? AND origin = ? AND created_at >= ?", userID, string(biz.UserSkillOriginAuto), since.UTC()).
			Count(&count).Error
	})
	if err != nil {
		return 0, catalogError("count auto user skills", err)
	}
	return int(count), nil
}
