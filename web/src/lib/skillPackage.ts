// SKILL 包解析：支持 .zip（含 SKILL.md）、Markdown 与 JSON 三种常见格式。

export interface ParsedSkillPackage {
  // 技能名称。
  name: string;
  // 一句话说明。
  description: string;
  // 会注入上下文的技能正文。
  promptTemplate: string;
  // 可选参数定义。
  parameters: Record<string, unknown>;
  // 未导入的二进制资源提示。
  notes: string[];
}

// MAX_REFERENCE_BYTES 限制参考资料总量，避免把压缩包内全部内容塞进提示词。
const MAX_REFERENCE_BYTES = 200 * 1024;

// readFileText 优先使用 File.text()，测试环境或旧浏览器回退到 FileReader。
function readFileText(file: File): Promise<string> {
  if (typeof file.text === "function") {
    return file.text();
  }
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result ?? ""));
    reader.onerror = () => reject(reader.error ?? new Error("读取文件失败"));
    reader.readAsText(file);
  });
}

// readFileBytes 读取二进制内容，供 ZIP 解包使用。
function readFileBytes(file: File): Promise<ArrayBuffer> {
  if (typeof file.arrayBuffer === "function") {
    return file.arrayBuffer();
  }
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(reader.result as ArrayBuffer);
    reader.onerror = () => reject(reader.error ?? new Error("读取文件失败"));
    reader.readAsArrayBuffer(file);
  });
}

// parseFrontmatter 解析简单的 YAML 头信息；只识别单行 key: value，避免引入完整 YAML 依赖。
function parseFrontmatter(content: string): { meta: Record<string, string>; body: string } {
  const normalized = content.replace(/^\uFEFF/, "");
  if (!normalized.startsWith("---")) {
    return { meta: {}, body: normalized };
  }
  const lines = normalized.split(/\r?\n/);
  const end = lines.findIndex((line, index) => index > 0 && line.trim() === "---");
  if (end < 0) {
    return { meta: {}, body: normalized };
  }
  const meta: Record<string, string> = {};
  for (const line of lines.slice(1, end)) {
    const match = line.match(/^([A-Za-z0-9_-]+)\s*:\s*(.+)$/);
    if (match) {
      meta[match[1].toLowerCase()] = match[2].trim().replace(/^["']|["']$/g, "");
    }
  }
  return { meta, body: lines.slice(end + 1).join("\n").trim() };
}

// deriveName 从文件名推导技能名称。
function deriveName(filename: string): string {
  return filename
    .replace(/\.(zip|md|markdown|txt|json)$/i, "")
    .replace(/[-_]+/g, " ")
    .trim()
    .slice(0, 128);
}

// trimText 截断到后端允许的长度。
function trimText(value: string, limit: number): string {
  return value.trim().slice(0, limit);
}

// readZip 解析 SKILL 包：优先读取 SKILL.md，其余文本作为参考资料合并。
async function readZip(file: File): Promise<ParsedSkillPackage> {
  // JSZip 体积较大，仅在真正上传 ZIP 时按需加载，避免拖慢首屏。
  const { default: JSZip } = await import("jszip");
  const zip = await JSZip.loadAsync(await readFileBytes(file));
  const entries = Object.values(zip.files).filter((entry) => !entry.dir);
  const markdowns = entries.filter((entry) => /\.md$/i.test(entry.name));
  const main = markdowns.find((entry) => /(^|\/)skill\.md$/i.test(entry.name)) ?? markdowns[0];
  if (!main) {
    throw new Error("压缩包内没有 SKILL.md 或 Markdown 文件。");
  }
  const mainText = await main.async("string");
  const parsed = parseFrontmatter(mainText);
  const notes: string[] = [];

  let body = parsed.body || mainText;
  const references = markdowns
    .filter((entry) => entry.name !== main.name)
    .sort((left, right) => left.name.localeCompare(right.name));
  let referenceBytes = 0;
  for (const entry of references) {
    if (referenceBytes >= MAX_REFERENCE_BYTES) {
      notes.push(`其余参考资料未导入（已达到 ${MAX_REFERENCE_BYTES / 1024}KB 上限）。`);
      break;
    }
    const content = await entry.async("string");
    referenceBytes += content.length;
    body += `\n\n## 参考资料：${entry.name}\n\n${content}`;
  }
  const binaries = entries.filter((entry) => !/\.(md|markdown|txt|json|yaml|yml)$/i.test(entry.name));
  if (binaries.length > 0) {
    notes.push(`压缩包内的 ${binaries.length} 个二进制资源不会上传，只保留文字说明。`);
  }

  return {
    name: trimText(parsed.meta.name ?? deriveName(file.name), 128),
    description: trimText(parsed.meta.description ?? parsed.body.split(/\r?\n/).find((line) => line.trim() && !line.startsWith("#")) ?? "", 512),
    promptTemplate: trimText(body, 256 * 1024),
    parameters: {},
    notes,
  };
}

// parseJSONPackage 支持后端字段名与 camelCase 两种写法。
function parseJSONPackage(text: string): ParsedSkillPackage {
  const raw = JSON.parse(text) as Record<string, unknown>;
  const source = typeof raw.skill === "object" && raw.skill !== null ? (raw.skill as Record<string, unknown>) : raw;
  const name = typeof source.name === "string" ? source.name : "";
  const description = typeof source.description === "string" ? source.description : "";
  const prompt = typeof source.prompt_template === "string" ? source.prompt_template : typeof source.promptTemplate === "string" ? source.promptTemplate : "";
  const parameters = typeof source.parameters === "object" && source.parameters !== null && !Array.isArray(source.parameters)
    ? (source.parameters as Record<string, unknown>)
    : {};
  if (!prompt.trim()) {
    throw new Error("JSON 中缺少 prompt_template 字段。");
  }
  return {
    name: trimText(name || "未命名技能", 128),
    description: trimText(description, 512),
    promptTemplate: trimText(prompt, 256 * 1024),
    parameters,
    notes: [],
  };
}

// parseSkillPackage 根据文件类型解析 SKILL 包。
export async function parseSkillPackage(file: File): Promise<ParsedSkillPackage> {
  const name = file.name.toLowerCase();
  if (name.endsWith(".json")) {
    return parseJSONPackage(await readFileText(file));
  }
  if (name.endsWith(".zip")) {
    return readZip(file);
  }
  if (name.endsWith(".md") || name.endsWith(".markdown") || name.endsWith(".txt")) {
    const parsed = parseFrontmatter(await readFileText(file));
    return {
      name: trimText(parsed.meta.name ?? deriveName(file.name), 128),
      description: trimText(
        parsed.meta.description ?? parsed.body.split(/\r?\n/).find((line) => line.trim() && !line.startsWith("#")) ?? "",
        512,
      ),
      promptTemplate: trimText(parsed.body, 256 * 1024),
      parameters: {},
      notes: [],
    };
  }
  throw new Error("只支持 .zip、.md、.markdown、.txt 或 .json 文件。");
}
