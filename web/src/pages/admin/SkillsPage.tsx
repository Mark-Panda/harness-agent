// Skill 模板管理：提示词模板与参数 Schema，管理员维护平台级能力。
import { useRef, useState } from "react";
import { Badge, Button, Callout, Card, ConfirmDialog, EmptyState, ErrorBanner, Field, Input, LoadingState, PageHeader, Select, Textarea } from "../../components/ui";
import { mutate, requestJSON } from "../../lib/api";
import { describeError } from "../../lib/errors";
import { parseSkillPackage } from "../../lib/skillPackage";
import { formatDateTime, statusLabel, statusTone } from "../../lib/format";
import { useRemote } from "../../lib/hooks";
import type { Page, Skill, SkillStatus } from "../../lib/types";

interface SkillFormState {
  name: string;
  description: string;
  promptTemplate: string;
  parameters: string;
  status: SkillStatus;
}

const emptySkill: SkillFormState = { name: "", description: "", promptTemplate: "", parameters: "{}", status: "draft" };

function SkillForm({ initial, onSaved, onCancel }: { initial: Skill | null; onSaved: () => void; onCancel: () => void }) {
  const [form, setForm] = useState<SkillFormState>(
    initial
      ? { name: initial.name, description: initial.description, promptTemplate: initial.prompt_template, parameters: JSON.stringify(initial.parameters ?? {}, null, 2), status: initial.status }
      : emptySkill,
  );
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      let parameters: Record<string, unknown>;
      try {
        parameters = JSON.parse(form.parameters || "{}") as Record<string, unknown>;
      } catch {
        throw new Error("参数 JSON 格式错误");
      }
      const body = {
        name: form.name.trim(),
        description: form.description.trim(),
        prompt_template: form.promptTemplate,
        parameters,
        status: form.status,
      };
      if (initial) {
        await mutate<Skill>(`/api/v1/skills/${initial.id}`, "PUT", body, crypto.randomUUID());
      } else {
        await mutate<Skill>("/api/v1/skills", "POST", body, crypto.randomUUID());
      }
      onSaved();
    } catch (submitError) {
      setError(submitError instanceof Error && submitError.message === "参数 JSON 格式错误" ? submitError.message : describeError(submitError));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card title={initial ? `编辑技能：${initial.name}` : "新建技能"} description="技能会作为额外说明交给助手。把它想成岗位手册，不要写密钥或个人隐私。">
      <form onSubmit={submit} className="grid gap-4 md:grid-cols-2">
        <Field label="技能名称" hint="例如“需求结构化”“代码审查清单”。">
          <Input value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} required maxLength={128} />
        </Field>
        <Field label="状态">
          <Select value={form.status} onChange={(event) => setForm({ ...form, status: event.target.value as SkillStatus })}>
            <option value="draft">草稿</option>
            <option value="published">发布</option>
            <option value="disabled">停用</option>
          </Select>
        </Field>
        <div className="md:col-span-2">
          <Field label="一句话说明" hint="告诉使用者在什么场景下选这个技能。">
            <Input value={form.description} onChange={(event) => setForm({ ...form, description: event.target.value })} maxLength={512} placeholder="例如：把零散需求整理成带优先级的清单" />
          </Field>
        </div>
        <div className="md:col-span-2">
          <Field label="技能说明（会发给助手）" hint="用自然语言写做事步骤、输出格式和禁忌。">
            <Textarea className="min-h-40 text-xs" value={form.promptTemplate} onChange={(event) => setForm({ ...form, promptTemplate: event.target.value })} required spellCheck={false} />
          </Field>
        </div>
        <div className="md:col-span-2">
          <Field label="参数定义（高级，可保持 {}）" hint="需要动态参数时才填写 JSON Schema。">
            <Textarea className="min-h-28 font-mono text-xs" value={form.parameters} onChange={(event) => setForm({ ...form, parameters: event.target.value })} spellCheck={false} />
          </Field>
        </div>
        {error ? <p role="alert" className="rounded-lg border border-rust bg-rust-soft px-3 py-2 text-xs text-rust md:col-span-2">{error}</p> : null}
        <div className="flex justify-end gap-2 md:col-span-2">
          <Button type="button" variant="secondary" onClick={onCancel}>取消</Button>
          <Button type="submit" loading={busy}>{initial ? "保存修改" : "创建技能"}</Button>
        </div>
      </form>
    </Card>
  );
}

export default function SkillsPage() {
  const skills = useRemote(() => requestJSON<Page<Skill>>("/api/v1/skills"), []);
  const [editing, setEditing] = useState<Skill | null | "new">(null);
  const [deleting, setDeleting] = useState<Skill | null>(null);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [importing, setImporting] = useState(false);
  const [importError, setImportError] = useState<string | null>(null);
  const [importNote, setImportNote] = useState<string | null>(null);
  const importRef = useRef<HTMLInputElement | null>(null);

  // importPackage 解析 SKILL 包后直接创建草稿并打开编辑器，用户确认后再发布。
  const importPackage = async (file: File) => {
    setImporting(true);
    setImportError(null);
    setImportNote(null);
    try {
      const parsed = await parseSkillPackage(file);
      const created = await mutate<Skill>("/api/v1/skills", "POST", {
        name: parsed.name,
        description: parsed.description,
        prompt_template: parsed.promptTemplate,
        parameters: parsed.parameters,
        status: "draft",
      }, crypto.randomUUID());
      skills.reload();
      setEditing(created);
      setImportNote(parsed.notes.length > 0 ? parsed.notes.join(" ") : `已导入「${created.name}」，请检查内容后发布。`);
    } catch (failure) {
      setImportError(failure instanceof Error && failure.message ? failure.message : describeError(failure));
    } finally {
      setImporting(false);
      if (importRef.current) {
        importRef.current.value = "";
      }
    }
  };

  const remove = async () => {
    if (!deleting) {
      return;
    }
    setBusy(true);
    setDeleteError(null);
    try {
      await mutate<void>(`/api/v1/skills/${deleting.id}`, "DELETE");
      setDeleting(null);
      skills.reload();
    } catch (failure) {
      setDeleteError(describeError(failure));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        eyebrow="平台设置 · 能力"
        title="技能"
        description="技能是一段可复用的做事说明。创建助手中可以勾选，让助手按你的习惯工作。"
        actions={
          <>
            <input
              ref={importRef}
              type="file"
              accept=".zip,.md,.markdown,.txt,.json"
              className="hidden"
              onChange={(event) => {
                const file = event.target.files?.[0];
                if (file) {
                  void importPackage(file);
                }
              }}
            />
            <Button variant="secondary" loading={importing} onClick={() => importRef.current?.click()}>
              导入 SKILL 包
            </Button>
            <Button onClick={() => setEditing("new")}>新建技能</Button>
          </>
        }
      />
      {skills.error ? <ErrorBanner message={skills.error} onRetry={skills.reload} /> : null}
      {importError ? <ErrorBanner message={importError} /> : null}
      {importNote ? <Callout tone="success">{importNote}</Callout> : null}
      {editing ? <SkillForm initial={editing === "new" ? null : editing} onCancel={() => setEditing(null)} onSaved={() => { setEditing(null); skills.reload(); }} /> : null}
      <Card title="已有技能" description={`共 ${skills.data?.items.length ?? 0} 个技能`}>
        {skills.loading ? (
          <LoadingState />
        ) : skills.data && skills.data.items.length > 0 ? (
          <div className="flex flex-col gap-3">
            {skills.data.items.map((skill) => (
              <article key={skill.id} className="rounded-lg border border-line bg-paper-deep/40 p-4">
                <div className="flex flex-wrap items-start justify-between gap-3">
                  <div className="min-w-0">
                    <div className="flex flex-wrap items-center gap-2">
                      <h3 className="text-sm font-semibold text-ink">{skill.name}</h3>
                      <Badge tone="neutral">v{skill.version}</Badge>
                      <Badge tone={statusTone(skill.status)}>{statusLabel(skill.status)}</Badge>
                    </div>
                    <p className="mt-1 text-xs text-muted">{skill.description || "无描述"}</p>
                    <p className="mt-1 text-[11px] text-muted">更新于 {formatDateTime(skill.updated_at)}</p>
                  </div>
                  <div className="flex gap-2">
                    <Button size="sm" variant="secondary" onClick={() => setEditing(skill)}>编辑</Button>
                    <Button size="sm" variant="danger" onClick={() => { setDeleting(skill); setDeleteError(null); }}>删除</Button>
                  </div>
                </div>
                <details className="mt-3 text-xs text-muted">
                  <summary className="cursor-pointer text-muted">查看提示词模板</summary>
                  <pre className="mt-2 overflow-x-auto whitespace-pre-wrap rounded border border-line bg-paper-raised p-3 font-mono text-[11px] text-ink-soft">{skill.prompt_template}</pre>
                </details>
              </article>
            ))}
          </div>
        ) : (
          <EmptyState title="还没有技能" description="创建一段做事说明，之后可以在助手中勾选使用。" action={<Button size="sm" onClick={() => setEditing("new")}>新建技能</Button>} />
        )}
      </Card>
      <ConfirmDialog
        open={deleting !== null}
        title={`删除 Skill ${deleting?.name ?? ""}？`}
        description="被已发布 Agent 引用的 Skill 无法删除。"
        confirmLabel="确认删除"
        busy={busy}
        onCancel={() => setDeleting(null)}
        onConfirm={() => void remove()}
      >
        {deleteError ? <p role="alert" className="text-xs text-rust">{deleteError}</p> : null}
      </ConfirmDialog>
    </div>
  );
}
