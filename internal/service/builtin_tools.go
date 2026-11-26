package service

// BuiltinTool 是平台内置工具的公开目录条目。
type BuiltinTool struct {
	// Name 工具名，与模型侧调用名一致。
	Name string `json:"name"`
	// Label 面向用户的短名称。
	Label string `json:"label"`
	// Description 工具用途说明。
	Description string `json:"description"`
	// RiskLevel 风险等级：low/medium/high。
	RiskLevel string `json:"risk_level"`
	// Enabled 平台是否启用该工具。
	Enabled bool `json:"enabled"`
}

// builtinToolCatalog 是平台支持的内置工具静态元数据。
var builtinToolCatalog = []BuiltinTool{
	{Name: "shell_exec", Label: "命令执行", Description: "在服务端 Shell 中执行命令", RiskLevel: "high"},
	{Name: "http_request", Label: "HTTP 请求", Description: "发起 HTTP/HTTPS 请求", RiskLevel: "high"},
	{Name: "fs_read_file", Label: "读取文件", Description: "读取用户工作区内的文本文件", RiskLevel: "low"},
	{Name: "fs_write_file", Label: "写入文件", Description: "写入或覆盖用户工作区文件", RiskLevel: "medium"},
	{Name: "fs_edit_file", Label: "编辑文件", Description: "替换用户工作区文件中的文本", RiskLevel: "medium"},
	{Name: "fs_list_dir", Label: "列举目录", Description: "列出用户工作区目录", RiskLevel: "low"},
	{Name: "fs_glob", Label: "文件查找", Description: "按 glob 模式查找文件", RiskLevel: "low"},
	{Name: "fs_grep", Label: "内容检索", Description: "按正则检索文件内容", RiskLevel: "low"},
}

// BuiltinToolCatalog 返回全部内置工具及启用状态；enabled 为 nil 时视为全部启用。
func BuiltinToolCatalog(enabled map[string]bool) []BuiltinTool {
	result := make([]BuiltinTool, 0, len(builtinToolCatalog))
	for _, tool := range builtinToolCatalog {
		if enabled == nil {
			tool.Enabled = true
		} else {
			tool.Enabled = enabled[tool.Name]
		}
		result = append(result, tool)
	}
	return result
}
