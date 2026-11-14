package biz

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/Mark-Panda/harness-agent/pkg/skillpkg"
	"github.com/google/uuid"
)

// SkillBundleFormat 是当前支持的标准技能包格式。
const SkillBundleFormat = "anthropic_skills"

// MaxSkillFileReadBytes 限制单次通过接口或工具读取的技能文件大小。
const MaxSkillFileReadBytes = 256 << 10

// 技能包相关错误。
var (
	// ErrSkillBundleNotFound 技能不存在或尚未导入技能包。
	ErrSkillBundleNotFound = errors.New("skill bundle not found")
	// ErrSkillBundleUnsupported 技能包内容不支持当前操作（如读取二进制文件或执行非脚本文件）。
	ErrSkillBundleUnsupported = errors.New("skill bundle operation is not supported")
)

// SkillBundleInfo 是技能包的元数据；零值表示该技能没有导入标准包。
type SkillBundleInfo struct {
	// Format 包格式，当前固定 anthropic_skills。
	Format string `json:"format"`
	// Entrypoint 入口文件，固定 SKILL.md。
	Entrypoint string `json:"entrypoint"`
	// FileCount 文件数量。
	FileCount int `json:"file_count"`
	// TotalBytes 解压后的总字节数。
	TotalBytes int64 `json:"total_bytes"`
	// HasScripts 是否包含可执行脚本。
	HasScripts bool `json:"has_scripts"`
	// ExecutionEnabled 是否允许执行包内脚本。
	ExecutionEnabled bool `json:"execution_enabled"`
	// Digest 目录树摘要。
	Digest string `json:"digest"`
}

// Empty 判断技能是否尚未导入技能包。
func (b SkillBundleInfo) Empty() bool {
	return b.FileCount == 0 && b.Format == ""
}

// SkillFileOwner 标识文件所属技能类型。
type SkillFileOwner string

const (
	// SkillFileOwnerPlatform 平台级技能，全局共享。
	SkillFileOwnerPlatform SkillFileOwner = "platform"
	// SkillFileOwnerUser 用户级技能，按 user_id 隔离。
	SkillFileOwnerUser SkillFileOwner = "user"
)

// SkillBundleFile 是技能包内的单个文件。
type SkillBundleFile struct {
	// Owner 所属技能类型。
	Owner SkillFileOwner
	// SkillID 所属技能 ID。
	SkillID uuid.UUID
	// SkillName 所属技能名称，运行时用于模型选择。
	SkillName string
	// Path 相对技能根目录的路径。
	Path string
	// Content 文件内容；列表接口可只返回元数据。
	Content []byte
	// MIME 媒体类型。
	MIME string
	// SizeBytes 文件字节数。
	SizeBytes int64
	// SHA256 内容摘要。
	SHA256 string
	// IsScript 是否为允许执行的脚本。
	IsScript bool
	// ExecutionEnabled 所属技能是否开启了脚本执行开关。
	ExecutionEnabled bool
}

// SkillBundleReader 是运行时读取已绑定技能包文件的端口。
type SkillBundleReader interface {
	// ListBoundSkillFiles 同时读取平台与用户技能包文件，用户数据必须按 userID 过滤。
	ListBoundSkillFiles(ctx context.Context, userID uuid.UUID, platformSkillIDs, userSkillIDs []uuid.UUID) ([]SkillBundleFile, error)
}

// UserSkillBundleRepository 定义用户技能包文件持久化契约。
type UserSkillBundleRepository interface {
	// ReplaceUserSkillBundle 在单事务内替换技能包文件并更新技能表 bundle_* 元数据。
	ReplaceUserSkillBundle(ctx context.Context, userID, skillID uuid.UUID, bundle SkillBundleInfo, files []SkillBundleFile, event OutboxEvent) error
	// ListUserSkillFileMeta 列出技能包文件元数据，不返回内容。
	ListUserSkillFileMeta(ctx context.Context, userID, skillID uuid.UUID) ([]SkillBundleFile, error)
	// GetUserSkillFile 读取单个文件，强制校验 user_id 与技能归属。
	GetUserSkillFile(ctx context.Context, userID, skillID uuid.UUID, path string) (SkillBundleFile, error)
	// SetUserSkillBundleExecution 更新脚本执行开关。
	SetUserSkillBundleExecution(ctx context.Context, userID, skillID uuid.UUID, enabled bool, event OutboxEvent) error
}

// SkillBundleRepository 定义平台技能包文件持久化契约，仅管理员可写。
type SkillBundleRepository interface {
	// ReplaceSkillBundle 在单事务内替换平台技能包文件并更新 bundle_* 元数据。
	ReplaceSkillBundle(ctx context.Context, actorID, skillID uuid.UUID, bundle SkillBundleInfo, files []SkillBundleFile, event OutboxEvent) error
	// ListSkillFileMeta 列出平台技能包文件元数据，不返回内容。
	ListSkillFileMeta(ctx context.Context, skillID uuid.UUID) ([]SkillBundleFile, error)
	// GetSkillFile 读取平台技能包单个文件。
	GetSkillFile(ctx context.Context, skillID uuid.UUID, path string) (SkillBundleFile, error)
	// SetSkillBundleExecution 更新平台技能脚本执行开关。
	SetSkillBundleExecution(ctx context.Context, actorID, skillID uuid.UUID, enabled bool, event OutboxEvent) error
}

// bundleFromPackage 把解析结果转为领域元数据与文件列表。
func bundleFromPackage(pkg *skillpkg.Package, owner SkillFileOwner, skillID uuid.UUID) (SkillBundleInfo, []SkillBundleFile) {
	info := SkillBundleInfo{
		Format: SkillBundleFormat, Entrypoint: pkg.Entrypoint,
		FileCount: len(pkg.Files), TotalBytes: pkg.TotalBytes,
		HasScripts: pkg.HasScripts, Digest: pkg.Digest,
	}
	files := make([]SkillBundleFile, 0, len(pkg.Files))
	for _, file := range pkg.Files {
		files = append(files, SkillBundleFile{
			Owner: owner, SkillID: skillID, Path: file.Path, Content: file.Content,
			MIME: file.MIME, SizeBytes: int64(len(file.Content)), SHA256: file.SHA256, IsScript: file.IsScript,
		})
	}
	return info, files
}

// validateSkillFileRead 校验读取路径并确保返回文本内容在大小上限内。
func validateSkillFileRead(file SkillBundleFile) error {
	if strings.TrimSpace(file.Path) == "" {
		return invalid("skill file path is required")
	}
	if len(file.Content) > MaxSkillFileReadBytes {
		return invalid("skill file exceeds the read limit")
	}
	if !skillpkg.IsTextMIME(file.MIME) || !utf8.Valid(file.Content) {
		return ErrSkillBundleUnsupported
	}
	return nil
}
