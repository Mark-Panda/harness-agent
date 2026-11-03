import JSZip from "jszip";
import { describe, expect, it } from "vitest";
import { parseSkillPackage } from "./skillPackage";

describe("parseSkillPackage", () => {
  it("parses Markdown frontmatter into skill fields", async () => {
    const file = new File([
      "---\nname: 需求整理\ndescription: 把零散需求整理为清单\n---\n\n先给结论，再列依据。",
    ], "skill.md", { type: "text/markdown" });
    const parsed = await parseSkillPackage(file);
    expect(parsed.name).toBe("需求整理");
    expect(parsed.description).toContain("零散需求");
    expect(parsed.promptTemplate).toContain("先给结论");
    expect(parsed.promptTemplate).not.toContain("name:");
  });

  it("parses JSON packages", async () => {
    const file = new File([
      JSON.stringify({ name: "代码审查", description: "按清单审查", prompt_template: "检查边界条件", parameters: { level: "string" } }),
    ], "skill.json", { type: "application/json" });
    const parsed = await parseSkillPackage(file);
    expect(parsed.name).toBe("代码审查");
    expect(parsed.parameters).toEqual({ level: "string" });
  });

  it("reads SKILL.md from a zip package and notes binary assets", async () => {
    const zip = new JSZip();
    zip.file("SKILL.md", "---\nname: 资料助手\ndescription: 汇总文档\n---\n\n先读资料再回答。");
    zip.file("references/guide.md", "## 指南\n\n引用时标注来源。");
    zip.file("assets/logo.png", new Uint8Array([1, 2, 3]));
    const bytes = await zip.generateAsync({ type: "uint8array" });
    const buffer = bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength) as ArrayBuffer;
    const file = new File([buffer], "skill-pack.zip", { type: "application/zip" });
    const parsed = await parseSkillPackage(file);
    expect(parsed.name).toBe("资料助手");
    expect(parsed.promptTemplate).toContain("参考资料：references/guide.md");
    expect(parsed.notes.join(" ")).toContain("二进制资源");
  });
});
