package biz

import (
	"strings"

	"github.com/Mark-Panda/harness-agent/pkg/skillpkg"
	"gopkg.in/yaml.v3"
)

// buildAutoSkillBundle 把自动技能转换为只含 SKILL.md 的最小标准技能包。
func buildAutoSkillBundle(skill UserSkill) (SkillBundleInfo, []SkillBundleFile, error) {
	prompt := strings.TrimSpace(skill.PromptTemplate)
	if prompt == "" {
		return SkillBundleInfo{}, nil, invalid("auto skill prompt template is required")
	}
	if !strings.HasPrefix(prompt, "# ") {
		prompt = "# " + strings.TrimSpace(skill.Name) + "\n\n" + prompt
	}
	meta, err := yaml.Marshal(struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}{Name: strings.TrimSpace(skill.Name), Description: strings.TrimSpace(skill.Description)})
	if err != nil {
		return SkillBundleInfo{}, nil, err
	}
	content := "---\n" + string(meta) + "---\n\n" + prompt + "\n"
	pkg, err := skillpkg.ParseMarkdown([]byte(content), skill.Name)
	if err != nil {
		return SkillBundleInfo{}, nil, err
	}
	info, files := bundleFromPackage(pkg, SkillFileOwnerUser, skill.ID)
	return info, files, nil
}
