package data

import (
	"context"
	"errors"
	"time"

	"github.com/Mark-Panda/harness-agent/internal/biz"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// skillFileModel 对应 skill_files 表：平台技能包文件。
type skillFileModel struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey"`
	SkillID   uuid.UUID `gorm:"type:uuid;not null"`
	Path      string    `gorm:"not null"`
	Content   []byte    `gorm:"type:bytea;not null"`
	MIME      string    `gorm:"not null"`
	SizeBytes int64     `gorm:"not null"`
	SHA256    string    `gorm:"not null"`
	IsScript  bool      `gorm:"not null"`
	CreatedAt time.Time `gorm:"not null"`
}

// TableName 指定平台技能文件表名。
func (skillFileModel) TableName() string { return "skill_files" }

// userSkillFileModel 对应 user_skill_files 表：用户技能包文件，按 user_id 隔离。
type userSkillFileModel struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey"`
	UserID    uuid.UUID `gorm:"type:uuid;not null"`
	SkillID   uuid.UUID `gorm:"type:uuid;not null"`
	Path      string    `gorm:"not null"`
	Content   []byte    `gorm:"type:bytea;not null"`
	MIME      string    `gorm:"not null"`
	SizeBytes int64     `gorm:"not null"`
	SHA256    string    `gorm:"not null"`
	IsScript  bool      `gorm:"not null"`
	CreatedAt time.Time `gorm:"not null"`
}

// TableName 指定用户技能文件表名。
func (userSkillFileModel) TableName() string { return "user_skill_files" }

// skillFileRow 是带技能名称的查询结果行。
type skillFileRow struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	SkillID   uuid.UUID
	SkillName string `gorm:"column:skill_name"`
	Path      string
	Content   []byte
	MIME      string `gorm:"column:mime"`
	SizeBytes int64  `gorm:"column:size_bytes"`
	SHA256    string `gorm:"column:sha256"`
	IsScript  bool   `gorm:"column:is_script"`
	// ExecutionEnabled 技能级脚本执行开关。
	ExecutionEnabled bool `gorm:"column:execution_enabled"`
	CreatedAt        time.Time
}

// SkillBundleRepository 同时实现平台与用户技能包持久化契约。
type SkillBundleRepository struct {
	db *gorm.DB
}

// NewSkillBundleRepository 构造技能包仓库。
func NewSkillBundleRepository(db *gorm.DB) *SkillBundleRepository {
	return &SkillBundleRepository{db: db}
}

// ReplaceUserSkillBundle 在用户作用域事务内替换技能包文件并更新元数据。
func (r *SkillBundleRepository) ReplaceUserSkillBundle(ctx context.Context, userID, skillID uuid.UUID, bundle biz.SkillBundleInfo, files []biz.SkillBundleFile, event biz.OutboxEvent) error {
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		var skill userSkillModel
		if err := tx.Where("id = ? AND user_id = ?", skillID, userID).First(&skill).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ? AND skill_id = ?", userID, skillID).Delete(&userSkillFileModel{}).Error; err != nil {
			return err
		}
		records := make([]userSkillFileModel, 0, len(files))
		now := time.Now().UTC()
		for _, file := range files {
			records = append(records, userSkillFileModel{
				ID: uuid.New(), UserID: userID, SkillID: skillID, Path: file.Path,
				Content: file.Content, MIME: file.MIME, SizeBytes: file.SizeBytes,
				SHA256: file.SHA256, IsScript: file.IsScript, CreatedAt: now,
			})
		}
		if len(records) > 0 {
			if err := tx.CreateInBatches(&records, 50).Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&userSkillModel{}).Where("id = ? AND user_id = ?", skillID, userID).Updates(bundleColumns(bundle)).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return catalogError("replace user skill bundle", err)
	}
	return nil
}

// ListUserSkillFileMeta 列出用户技能包文件元数据，不返回内容。
func (r *SkillBundleRepository) ListUserSkillFileMeta(ctx context.Context, userID, skillID uuid.UUID) ([]biz.SkillBundleFile, error) {
	rows := make([]skillFileRow, 0)
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Table("user_skill_files AS f").
			Select("f.id, f.user_id, f.skill_id, s.name AS skill_name, f.path, f.mime, f.size_bytes, f.sha256, f.is_script, f.created_at").
			Joins("JOIN user_skills s ON s.id = f.skill_id AND s.user_id = f.user_id").
			Where("f.user_id = ? AND f.skill_id = ?", userID, skillID).
			Order("f.path").Scan(&rows).Error
	})
	if err != nil {
		return nil, catalogError("list user skill file meta", err)
	}
	return toBundleFiles(rows, biz.SkillFileOwnerUser), nil
}

// GetUserSkillFile 读取用户技能包单个文件。
func (r *SkillBundleRepository) GetUserSkillFile(ctx context.Context, userID, skillID uuid.UUID, filePath string) (biz.SkillBundleFile, error) {
	var row skillFileRow
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		return tx.Table("user_skill_files AS f").
			Select("f.id, f.user_id, f.skill_id, s.name AS skill_name, f.path, f.content, f.mime, f.size_bytes, f.sha256, f.is_script, f.created_at").
			Joins("JOIN user_skills s ON s.id = f.skill_id AND s.user_id = f.user_id").
			Where("f.user_id = ? AND f.skill_id = ? AND f.path = ?", userID, skillID, filePath).
			Take(&row).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return biz.SkillBundleFile{}, biz.ErrSkillBundleNotFound
	}
	if err != nil {
		return biz.SkillBundleFile{}, catalogError("get user skill file", err)
	}
	files := toBundleFiles([]skillFileRow{row}, biz.SkillFileOwnerUser)
	return files[0], nil
}

// SetUserSkillBundleExecution 更新用户技能脚本执行开关。
func (r *SkillBundleRepository) SetUserSkillBundleExecution(ctx context.Context, userID, skillID uuid.UUID, enabled bool, event biz.OutboxEvent) error {
	err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
		result := tx.Model(&userSkillModel{}).Where("id = ? AND user_id = ?", skillID, userID).
			Updates(map[string]any{"bundle_execution_enabled": enabled, "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return catalogError("set user skill bundle execution", err)
	}
	return nil
}

// ReplaceSkillBundle 在 system scope 事务内替换平台技能包文件并更新元数据。
func (r *SkillBundleRepository) ReplaceSkillBundle(ctx context.Context, actorID, skillID uuid.UUID, bundle biz.SkillBundleInfo, files []biz.SkillBundleFile, event biz.OutboxEvent) error {
	err := WithScope(ctx, r.db, Scope{System: true, UserID: actorID}, func(tx *gorm.DB) error {
		var skill skillRecordModel
		if err := tx.Where("id = ?", skillID).First(&skill).Error; err != nil {
			return err
		}
		if err := tx.Where("skill_id = ?", skillID).Delete(&skillFileModel{}).Error; err != nil {
			return err
		}
		records := make([]skillFileModel, 0, len(files))
		now := time.Now().UTC()
		for _, file := range files {
			records = append(records, skillFileModel{
				ID: uuid.New(), SkillID: skillID, Path: file.Path, Content: file.Content,
				MIME: file.MIME, SizeBytes: file.SizeBytes, SHA256: file.SHA256,
				IsScript: file.IsScript, CreatedAt: now,
			})
		}
		if len(records) > 0 {
			if err := tx.CreateInBatches(&records, 50).Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&skillRecordModel{}).Where("id = ?", skillID).Updates(bundleColumns(bundle)).Error; err != nil {
			return err
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return catalogError("replace skill bundle", err)
	}
	return nil
}

// ListSkillFileMeta 列出平台技能包文件元数据，不返回内容。
func (r *SkillBundleRepository) ListSkillFileMeta(ctx context.Context, skillID uuid.UUID) ([]biz.SkillBundleFile, error) {
	rows := make([]skillFileRow, 0)
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Table("skill_files AS f").
			Select("f.id, f.skill_id, s.name AS skill_name, f.path, f.mime, f.size_bytes, f.sha256, f.is_script, f.created_at").
			Joins("JOIN skills s ON s.id = f.skill_id").
			Where("f.skill_id = ?", skillID).Order("f.path").Scan(&rows).Error
	})
	if err != nil {
		return nil, catalogError("list skill file meta", err)
	}
	return toBundleFiles(rows, biz.SkillFileOwnerPlatform), nil
}

// GetSkillFile 读取平台技能包单个文件。
func (r *SkillBundleRepository) GetSkillFile(ctx context.Context, skillID uuid.UUID, filePath string) (biz.SkillBundleFile, error) {
	var row skillFileRow
	err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
		return tx.Table("skill_files AS f").
			Select("f.id, f.skill_id, s.name AS skill_name, f.path, f.content, f.mime, f.size_bytes, f.sha256, f.is_script, f.created_at").
			Joins("JOIN skills s ON s.id = f.skill_id").
			Where("f.skill_id = ? AND f.path = ?", skillID, filePath).Take(&row).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return biz.SkillBundleFile{}, biz.ErrSkillBundleNotFound
	}
	if err != nil {
		return biz.SkillBundleFile{}, catalogError("get skill file", err)
	}
	files := toBundleFiles([]skillFileRow{row}, biz.SkillFileOwnerPlatform)
	return files[0], nil
}

// SetSkillBundleExecution 更新平台技能脚本执行开关。
func (r *SkillBundleRepository) SetSkillBundleExecution(ctx context.Context, actorID, skillID uuid.UUID, enabled bool, event biz.OutboxEvent) error {
	err := WithScope(ctx, r.db, Scope{System: true, UserID: actorID}, func(tx *gorm.DB) error {
		result := tx.Model(&skillRecordModel{}).Where("id = ?", skillID).
			Updates(map[string]any{"bundle_execution_enabled": enabled, "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return appendOutbox(tx, event)
	})
	if err != nil {
		return catalogError("set skill bundle execution", err)
	}
	return nil
}

// ListBoundSkillFiles 读取运行时所需的平台与用户技能包文件（含内容）。
func (r *SkillBundleRepository) ListBoundSkillFiles(ctx context.Context, userID uuid.UUID, platformSkillIDs, userSkillIDs []uuid.UUID) ([]biz.SkillBundleFile, error) {
	result := make([]biz.SkillBundleFile, 0)
	if len(platformSkillIDs) > 0 {
		rows := make([]skillFileRow, 0)
		err := WithScope(ctx, r.db, Scope{System: true}, func(tx *gorm.DB) error {
			return tx.Table("skill_files AS f").
				Select("f.id, f.skill_id, s.name AS skill_name, f.path, f.content, f.mime, f.size_bytes, f.sha256, f.is_script, s.bundle_execution_enabled AS execution_enabled, f.created_at").
				Joins("JOIN skills s ON s.id = f.skill_id").
				Where("f.skill_id IN ?", platformSkillIDs).Order("f.skill_id, f.path").Scan(&rows).Error
		})
		if err != nil {
			return nil, catalogError("list bound platform skill files", err)
		}
		result = append(result, toBundleFiles(rows, biz.SkillFileOwnerPlatform)...)
	}
	if len(userSkillIDs) > 0 && userID != uuid.Nil {
		rows := make([]skillFileRow, 0)
		err := WithScope(ctx, r.db, Scope{UserID: userID}, func(tx *gorm.DB) error {
			return tx.Table("user_skill_files AS f").
				Select("f.id, f.user_id, f.skill_id, s.name AS skill_name, f.path, f.content, f.mime, f.size_bytes, f.sha256, f.is_script, s.bundle_execution_enabled AS execution_enabled, f.created_at").
				Joins("JOIN user_skills s ON s.id = f.skill_id AND s.user_id = f.user_id").
				Where("f.user_id = ? AND f.skill_id IN ?", userID, userSkillIDs).
				Order("f.skill_id, f.path").Scan(&rows).Error
		})
		if err != nil {
			return nil, catalogError("list bound user skill files", err)
		}
		result = append(result, toBundleFiles(rows, biz.SkillFileOwnerUser)...)
	}
	return result, nil
}

// bundleColumns 构造技能表 bundle_* 列的更新集合。
func bundleColumns(bundle biz.SkillBundleInfo) map[string]any {
	return map[string]any{
		"bundle_format": bundle.Format, "bundle_entrypoint": bundle.Entrypoint,
		"bundle_file_count": bundle.FileCount, "bundle_total_bytes": bundle.TotalBytes,
		"bundle_has_scripts": bundle.HasScripts, "bundle_execution_enabled": bundle.ExecutionEnabled,
		"bundle_digest": bundle.Digest, "updated_at": time.Now().UTC(),
	}
}

// toBundleFiles 把查询行转换为领域文件模型。
func toBundleFiles(rows []skillFileRow, owner biz.SkillFileOwner) []biz.SkillBundleFile {
	files := make([]biz.SkillBundleFile, 0, len(rows))
	for _, row := range rows {
		files = append(files, biz.SkillBundleFile{
			Owner: owner, SkillID: row.SkillID, SkillName: row.SkillName, Path: row.Path,
			Content: row.Content, MIME: row.MIME, SizeBytes: row.SizeBytes,
			SHA256: row.SHA256, IsScript: row.IsScript, ExecutionEnabled: row.ExecutionEnabled,
		})
	}
	return files
}
