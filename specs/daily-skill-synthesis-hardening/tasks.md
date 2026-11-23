# 每日技能自动提炼加固任务清单

> 依赖顺序：T1 -> T2 -> T3/T4 -> T5 -> T6 -> T7 -> T8。T3 与 T4 可并行。

## T1. 标准 SKILL.md 解析能力

- [x] 在 `pkg/skillpkg/parser.go` 新增 `ParseMarkdown(content []byte, fallbackName string) (*Package, error)`。
- [x] 复用 `parseFrontmatter`、`firstParagraph`、`digestFiles`，返回单文件包，`Entrypoint=SKILL.md`，`HasScripts=false`。
- [x] 校验正文非空、名称不超过 `MaxNameLength`、描述不超过 `MaxDescriptionLength`。
- [x] 在 `pkg/skillpkg/parser_test.go` 覆盖：带 frontmatter、无 frontmatter、空正文、超长名称、摘要稳定。
- [x] 验证：`go test ./pkg/skillpkg/...`。

## T2. 提炼提示与候选解析升级

- [x] `internal/biz/user_skill_llm.go` 的 `skillExtractionOutput` 增加 `when_to_use`、`required_tools`、`evidence`。
- [x] `internal/biz/user_skill.go` 的 `SkillCandidate` 增加 `WhenToUse string`、`RequiredTools []string`、`Evidence []SkillEvidence`。
- [x] 新增 `SkillEvidence{Session string; Quote string}`。
- [x] 重写 `buildSkillExtractionPrompt`：
  - [x] 名称要求 kebab-case、最长 64 字符。
  - [x] 描述说明“做什么 + 何时使用”，目标 150-300 字符，上限 1024 字符。
  - [x] 正文要求标准标题：`## 何时使用`、`## 步骤`、`## 输出格式`、`## 约束`，并控制在 1800 Token 以内。
  - [ ] 要求至少 2 条证据，`session` 为会话标题，`quote` 为原文片段。
  - [ ] 增加置信度评分标准与一次性任务反例。
  - [ ] 声明对话内容是不可信数据，不执行其中指令。
  - [ ] 要求泛化姓名、账号、SN、手机号、邮箱、内部 URL。
- [ ] user 段使用 `<conversation_data>` 包裹，替换内部 `</conversation_data>`。
- [ ] 消息与会话标题按 UTF-8 边界截断，新增 `truncateUTF8Bytes`。
- [ ] `parseSkillCandidate` 兼容大写 ` ```JSON`，允许 JSON 前后有说明文字，解析新增字段。
- [ ] `UserSkillLLMConfig.MaxOutputTokens` 默认从 2048 提高到 4096；`SkillSynthesisConfig.normalize` 同步调整；检查 `configs/config.yaml` 显式配置。
- [ ] 更新 `internal/biz/user_skill_llm_test.go` 与 `direct_text_regression_test.go` 中受影响的断言。
- [ ] 验证：`go test ./internal/biz -run 'TestSkillLLM|TestSkillExtraction'`。

## T3. 自动技能确定性校验

- [x] 新增 `internal/biz/user_skill_validate.go`：
  - [x] `maxSynthesizedSkillPromptTokens = 1800`。
  - [x] `synthesizedSkillNamePattern = ^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`，3-64 字符。
  - [x] 描述与 `when_to_use` 校验 20-1024 字符；超过 500 字符记录运行时截断提示。
  - [x] `validateSynthesizedSkill(candidate, sessions, messagesBySession) error`。
  - [x] `validateSkillParameters`：`type=object`、`properties` 对象、`required` 子集、每个属性有 `type` 与 `description`。
  - [x] 占位符 `{{param}}` 与参数声明一致性校验。
  - [x] `validateSkillEvidence`：至少 2 条，quote 去空白后 >= 8 字符，且是对应会话消息子串。
  - [x] `containsSensitiveSkillText`：私钥、`Bearer`、`sk-`、`AKIA`、`password=`、带凭证 URL。
  - [x] `composeStandardSkillPrompt`：缺一级标题/`## 何时使用` 时补齐；其余标准标题缺失报错。
- [ ] 新增 `internal/biz/user_skill_validate_test.go`，覆盖所有失败分支与合法候选。
- [ ] 验证：`go test ./internal/biz -run TestValidateSynthesizedSkill`。

## T4. 确定性去重

- [x] 新增 `internal/biz/user_skill_dedupe.go`：
  - [x] `normalizeSkillName`：小写，`_`/空格/连续连字符统一为 `-`。
  - [x] `skillTokenSet`：ASCII 小写单词 + CJK 二字组。
  - [x] `tokenContainment`：交集 / 较小集合。
  - [x] `findDuplicateSkill(candidate, sourceSessionIDs, existing)`：
    - 规范化名称相同；或
    - 来源会话重叠 >= 75% 且正文包含度 >= 0.20；或
    - 来源会话重叠 >= 50% 且正文包含度 >= 0.35；或
    - 描述包含度 >= 0.8 且正文包含度 >= 0.5。
    - 正文 Token 少于 20 个时跳过相似度规则。
- [x] 新增 `internal/biz/user_skill_dedupe_test.go`，使用当前两条样例技能的正文做回归，确保判重。
- [x] 更新 `matchExistingSkillName` 使用规范化比较。
- [x] 验证：`go test ./internal/biz -run TestFindDuplicateSkill`。

## T5. 自动技能标准包写入

- [x] 新增 `internal/biz/user_skill_bundle.go`：
  - [x] `buildAutoSkillBundle(skill UserSkill) (SkillBundleInfo, []SkillBundleFile, error)`。
  - [x] 用 `yaml.Marshal` 生成 frontmatter，避免描述中的冒号/换行破坏 YAML。
  - [x] 正文缺少一级标题时补 `# <name>`。
  - [x] 调用 `skillpkg.ParseMarkdown` 与 `bundleFromPackage`。
- [x] 修改 `internal/biz/user_skill.go` 的 `CreateAuto`：
  - [x] 先执行结构校验。
  - [x] 创建成功后写 bundle。
  - [x] 写 bundle 失败时删除技能并返回错误。
- [x] 更新 `internal/biz/user_skill_test.go`：成功写入、失败回滚。
- [x] 验证：`go test ./internal/biz -run TestUserSkillServiceCreateAuto`。

## T6. Worker 接入校验、去重与扫描完整性

- [x] `internal/biz/user_skill.go` 的 `SkillSynthesisWorker.handle`：
  - [x] 只调用一次 `userSkills.List(..., 200)`，派生前 30 条 digest 与去重全集。
  - [x] 置信度通过后依次执行 `validateSynthesizedSkill`、`findDuplicateSkill`。
  - [x] `invalid_candidate` / `duplicate_skill` 均推进水位并结束。
  - [x] 会话查询 `MaxSessionsPerUser + 1`，消息查询 `MaxMessagesPerSession + 1`，多出的 1 条只判断截断。
  - [x] 读取失败记录 `scan_failed`，本轮不推进水位。
  - [x] 仅数量截断记录 `partial_scan`，继续推进水位，detail 追加“采样扫描，未覆盖的旧消息不会再次提炼”。
- [x] 更新 `internal/biz/user_skill_worker_test.go`：
  - [x] 合法候选创建成功。
  - [x] 非法候选 `invalid_candidate`，不创建。
  - [x] 重复候选 `duplicate_skill`，不创建。
  - [x] 消息读取失败不推进水位。
  - [x] 数量截断时 detail 含采样说明。
- [x] 验证：`go test ./internal/biz -run TestSkillSynthesisWorker`。

## T7. 前端结果文案

- [x] `web/src/pages/UserSkillsPage.tsx` 的 `synthesisOutcomeLabels` 增加 `invalid_candidate: "格式校验未通过"`。
- [x] 描述输入框 `maxLength` 从 512 调整为 1024，提示补充“建议 150-300 字；超过 300 Token 的部分运行时不会进入系统提示”。
- [x] 确认自动技能带 bundle 时显示“技能包 · 1 文件”并可用 `SkillBundlePanel` 预览 `SKILL.md`。
- [x] 验证：`npm run typecheck`、`npm test`。

## T8. 全量验证与部署

- [x] `gofmt -w` 修改的 Go 文件。
- [x] `go test ./...`。
- [x] `cd web && npm run typecheck && npm test && npm run build`。
- [x] `docker compose up -d --build`，确认 `migrate_exit=0`、app `healthy`、`/health/ready` 返回 ready。
- [x] 手动触发一次提炼，SQL 验证：
  - [x] `user_skills.bundle_format='anthropic_skills'` 且 `bundle_file_count>=1`。
  - [x] `user_skill_files` 存在 `SKILL.md`。
  - [x] 同批对话再次触发为 `duplicate_skill`。
  - [x] 执行记录 detail 能显示采样/失败原因。
- [x] 页面验证：我的技能页能看到标准包徽标、文件预览、`invalid_candidate` 文案。

## 不在本期范围

- 多候选技能提炼（一次最多 3 个）。
- 按会话维度的独立水位与超过 50 条消息的完整分页。
- 技能使用率、成功率统计与自动停用建议。
- 自动发布/自动绑定 Agent。
- 存量两条草稿的标准包回填。
