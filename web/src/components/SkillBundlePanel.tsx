// SkillBundlePanel 展示标准技能包的文件树、文本预览与脚本执行开关。
// basePath 区分平台技能（/api/v1/skills）与用户技能（/api/v1/user-skills）。
import { useState } from "react";
import { useToast } from "./Toast";
import { Badge, Button, Callout, ErrorBanner, LoadingState } from "./ui";
import { mutate, requestJSON } from "../lib/api";
import { describeError } from "../lib/errors";
import { useRemote } from "../lib/hooks";
import type { SkillBundleFile, SkillBundleFileContent, SkillBundleInfo } from "../lib/types";

interface SkillBundlePanelProps {
  basePath: string;
  skillId: string;
  skillName: string;
  bundle: SkillBundleInfo;
  onClose: () => void;
  onExecutionChanged: () => void;
}

// formatBytes 把字节数转成易读大小。
function formatBytes(value: number): string {
  if (value < 1024) return `${value} B`;
  if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KB`;
  return `${(value / (1024 * 1024)).toFixed(1)} MB`;
}

export function SkillBundlePanel({ basePath, skillId, skillName, bundle, onClose, onExecutionChanged }: SkillBundlePanelProps) {
  const toast = useToast();
  const files = useRemote(() => requestJSON<{ files: SkillBundleFile[] }>(`${basePath}/${skillId}/bundle`), [basePath, skillId]);
  const [selected, setSelected] = useState("");
  const preview = useRemote<SkillBundleFileContent | null>(
    () => (selected ? requestJSON<SkillBundleFileContent>(`${basePath}/${skillId}/bundle/file?path=${encodeURIComponent(selected)}`) : Promise.resolve(null)),
    [basePath, skillId, selected],
  );
  const [enabled, setEnabled] = useState(bundle.execution_enabled);
  const [saving, setSaving] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

  const toggleExecution = async () => {
    const next = !enabled;
    setSaving(true);
    setActionError(null);
    try {
      await mutate(`${basePath}/${skillId}/bundle/execution`, "PUT", { enabled: next }, crypto.randomUUID());
      setEnabled(next);
      toast.show(next ? "已允许执行脚本（仍需逐次审批）" : "已关闭脚本执行", "success");
      onExecutionChanged();
    } catch (failure) {
      setActionError(describeError(failure));
    } finally {
      setSaving(false);
    }
  };

  return (
    <section className="rounded-lg border border-line bg-paper-raised p-4" aria-label={`${skillName} 技能包文件`}>
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <h3 className="text-sm font-semibold text-ink">技能包文件 · {skillName}</h3>
          <p className="mt-1 text-[11px] text-muted">
            {bundle.entrypoint} · {bundle.file_count} 个文件 · {formatBytes(bundle.total_bytes)} · 摘要 {bundle.digest.slice(0, 12)}
          </p>
        </div>
        <div className="flex items-center gap-2">
          {bundle.has_scripts ? <Badge tone="warning">含脚本</Badge> : <Badge tone="neutral">无脚本</Badge>}
          <Button size="sm" variant="secondary" onClick={onClose}>收起</Button>
        </div>
      </div>

      {bundle.has_scripts ? (
        <div className="mt-3">
          <Callout tone="warning" title="脚本执行仅限开发环境">
            脚本以本机进程运行，默认关闭。即使开启，每次执行仍需要人工审批；生产环境不允许本机执行。
          </Callout>
          <label className="mt-3 flex items-start gap-3 rounded-lg border border-line bg-paper-deep/40 p-3">
            <input
              type="checkbox"
              checked={enabled}
              disabled={saving}
              onChange={() => void toggleExecution()}
              className="mt-0.5 size-4 accent-[var(--color-accent)]"
            />
            <span className="min-w-0">
              <span className="block text-sm font-medium text-ink">允许 Agent 执行该技能包内的脚本</span>
              <span className="mt-0.5 block text-xs leading-5 text-muted">关闭后 Agent 只能读取文件，无法发起脚本执行。</span>
            </span>
          </label>
        </div>
      ) : null}

      {actionError ? <ErrorBanner message={actionError} /> : null}

      {files.loading && !files.data ? (
        <LoadingState />
      ) : files.error ? (
        <ErrorBanner message={files.error} onRetry={files.reload} />
      ) : (
        <div className="mt-3 grid gap-3 lg:grid-cols-[minmax(0,18rem)_minmax(0,1fr)]">
          <ul className="max-h-72 overflow-auto rounded border border-line bg-paper-deep/40 p-2">
            {(files.data?.files ?? []).map((file) => (
              <li key={file.path}>
                <button
                  type="button"
                  onClick={() => setSelected(file.path)}
                  className={`flex w-full items-center justify-between gap-2 rounded px-2 py-1.5 text-left font-mono text-[11px] ${selected === file.path ? "bg-paper-raised text-ink" : "text-ink-soft hover:bg-paper-raised"}`}
                >
                  <span className="min-w-0 truncate">{file.path}</span>
                  <span className="flex shrink-0 items-center gap-1">
                    {file.is_script ? <Badge tone="warning">脚本</Badge> : null}
                    <span className="text-[10px] text-muted">{formatBytes(file.size)}</span>
                  </span>
                </button>
              </li>
            ))}
            {(files.data?.files ?? []).length === 0 ? <li className="px-2 py-1 text-xs text-muted">技能包没有文件</li> : null}
          </ul>
          <div className="min-h-32 rounded border border-line bg-paper-deep/40 p-3">
            {!selected ? (
              <p className="text-xs text-muted">选择左侧文件查看文本内容；二进制文件不支持预览。</p>
            ) : preview.loading && !preview.data ? (
              <LoadingState />
            ) : preview.error ? (
              <ErrorBanner message={preview.error} onRetry={preview.reload} />
            ) : (
              <>
                <p className="mb-2 break-all font-mono text-[11px] text-muted">{preview.data?.path}</p>
                <pre className="max-h-64 overflow-auto whitespace-pre-wrap font-mono text-[11px] text-ink-soft">{preview.data?.content}</pre>
              </>
            )}
          </div>
        </div>
      )}
    </section>
  );
}
