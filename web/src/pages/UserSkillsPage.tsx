// 我的技能：展示每日对话提炼与手动创建的用户级技能，支持编辑草稿、发布、停用与删除。
import { useEffect, useRef, useState } from "react";
import { FormDrawer } from "../components/FormDrawer";
import { SkillBundlePanel } from "../components/SkillBundlePanel";
import { useToast } from "../components/Toast";
import { Badge, Button, Callout, Card, ConfirmDialog, EmptyState, ErrorBanner, Field, Input, LoadingState, MenuItem, PageHeader, Pager, RowMenu, Select, Textarea } from "../components/ui";
import { mutate, requestJSON, uploadSkillPackage } from "../lib/api";
import { describeError } from "../lib/errors";
import { formatDateTime, statusLabel, statusTone } from "../lib/format";
import { useClientPage, useRemote } from "../lib/hooks";
import type { UserSkill, UserSkillInput, UserSkillOrigin, UserSkillRevision, UserSkillSynthesisPolicy, UserSkillSynthesisRun, UserSkillSynthesisSettings, UserSkillSynthesisSettingsInput } from "../lib/types";

interface UserSkillList {
  items: UserSkill[];
}

interface UserSkillFormState {
  name: string;
  description: string;
  promptTemplate: string;
  parameters: string;
}

const emptyForm: UserSkillFormState = { name: "", description: "", promptTemplate: "", parameters: "{}" };

// originLabel 把技能来源翻译成用户语言。
function originLabel(origin: UserSkillOrigin): string {
  return origin === "auto" ? "自动提炼" : "手动创建";
}

function UserSkillForm({ initial, onSaved, onCancel }: { initial: UserSkill | null; onSaved: () => void; onCancel: () => void }) {
  const initialForm: UserSkillFormState = initial
    ? {
        name: initial.name,
        description: initial.description,
        promptTemplate: initial.prompt_template,
        parameters: JSON.stringify(initial.parameters ?? {}, null, 2),
      }
    : emptyForm;
  const [form, setForm] = useState(initialForm);
  const toast = useToast();
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      let parameters: Record<string, unknown>;
      try {
        const parsed: unknown = JSON.parse(form.parameters || "{}");
        if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
          throw new Error("参数必须是 JSON 对象");
        }
        parameters = parsed as Record<string, unknown>;
      } catch (parseError) {
        throw new Error(parseError instanceof Error && parseError.message === "参数必须是 JSON 对象" ? parseError.message : "参数 JSON 格式错误");
      }
      const body: UserSkillInput = {
        name: form.name.trim(),
        description: form.description.trim(),
        prompt_template: form.promptTemplate,
        parameters,
      };
      if (initial) {
        // 名称由服务端保留，编辑只提交正文与参数。
        await mutate<UserSkill>(`/api/v1/user-skills/${initial.id}`, "PUT", body, crypto.randomUUID());
      } else {
        await mutate<UserSkill>("/api/v1/user-skills", "POST", body, crypto.randomUUID());
      }
      toast.show(initial ? "修改已保存" : "已创建技能草稿", "success");
      onSaved();
    } catch (submitError) {
      setError(submitError instanceof Error && (submitError.message === "参数 JSON 格式错误" || submitError.message === "参数必须是 JSON 对象")
        ? submitError.message
        : describeError(submitError));
    } finally {
      setBusy(false);
    }
  };

  return (
    <FormDrawer
      title={initial ? `编辑技能：${initial.name}` : "新建技能"}
      description="技能会作为额外说明交给助手。待确认的草稿可以修改，发布后正文不可原地编辑。"
      submitLabel={initial ? "保存修改" : "创建草稿"}
      busy={busy}
      dirty={JSON.stringify(form) !== JSON.stringify(initialForm)}
      error={error}
      onSubmit={submit}
      onCancel={onCancel}
    >
      <Field label="技能名称" hint={initial ? "名称创建后保持不变。" : "例如“周报整理”“需求结构化”。"}>
        <Input
          value={form.name}
          onChange={(event) => setForm({ ...form, name: event.target.value })}
          required
          maxLength={128}
          disabled={initial !== null}
        />
      </Field>
      <Field label="来源">
        <Input value={initial ? originLabel(initial.origin) : "手动创建"} disabled />
      </Field>
      <div className="sm:col-span-2">
        <Field label="一句话说明" hint="告诉使用者在什么场景下选这个技能；建议 150-300 字，超过 300 Token 的部分运行时不会进入系统提示。">
          <Input value={form.description} onChange={(event) => setForm({ ...form, description: event.target.value })} maxLength={1024} placeholder="例如：把零散需求整理成带优先级的清单" />
        </Field>
      </div>
      <div className="sm:col-span-2">
        <Field label="技能说明（会发给助手）" hint="用自然语言写做事步骤、输出格式和禁忌。">
          <Textarea className="min-h-40 text-xs" value={form.promptTemplate} onChange={(event) => setForm({ ...form, promptTemplate: event.target.value })} required spellCheck={false} />
        </Field>
      </div>
      <div className="sm:col-span-2">
        <Field label="参数定义（高级，可保持 {}）" hint="需要动态参数时才填写 JSON 对象。">
          <Textarea className="min-h-28 font-mono text-xs" value={form.parameters} onChange={(event) => setForm({ ...form, parameters: event.target.value })} spellCheck={false} />
        </Field>
      </div>
    </FormDrawer>
  );
}

// formatSavedAt 过滤未保存记录的零值时间，避免显示公元 1 年。
function formatSavedAt(value: string | null | undefined): string {
  return value && !value.startsWith("0001-01-01") ? formatDateTime(value) : "";
}

// SynthesisSettingsCard 配置每日技能提炼开关与执行提炼的模型策略。
function SynthesisSettingsCard() {
  const settings = useRemote(() => requestJSON<UserSkillSynthesisSettings>("/api/v1/user-skill-synthesis/settings"), []);
  const policies = useRemote(() => requestJSON<{ items: UserSkillSynthesisPolicy[] }>("/api/v1/user-skill-synthesis/policies"), []);
  const toast = useToast();
  const [enabled, setEnabled] = useState(false);
  const [policyID, setPolicyID] = useState("");
  const [initialized, setInitialized] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [savedAt, setSavedAt] = useState("");

  useEffect(() => {
    if (!settings.data || initialized) return;
    setEnabled(settings.data.enabled);
    setPolicyID(settings.data.model_policy_id ?? "");
    setSavedAt(formatSavedAt(settings.data.updated_at));
    setInitialized(true);
  }, [settings.data, initialized]);

  const publishedPolicies = policies.data?.items ?? [];
  const selectedPolicy = policyID ? publishedPolicies.find((policy) => policy.id === policyID) : undefined;
  const staleSelection = policyID !== "" && policies.data !== null && selectedPolicy === undefined;

  const save = async () => {
    setBusy(true);
    setError(null);
    try {
      if (staleSelection) {
        throw new Error("当前选择的策略已不可用，请重新选择后再保存。");
      }
      const policy = publishedPolicies.find((item) => item.id === policyID);
      const body: UserSkillSynthesisSettingsInput = {
        enabled,
        model_policy_id: policy?.id ?? null,
        model_policy_version: policy?.version ?? null,
      };
      const updated = await mutate<UserSkillSynthesisSettings>("/api/v1/user-skill-synthesis/settings", "PUT", body, crypto.randomUUID());
      setEnabled(updated.enabled);
      setPolicyID(updated.model_policy_id ?? "");
      setSavedAt(formatSavedAt(updated.updated_at));
      toast.show("自动提炼设置已保存", "success");
    } catch (failure) {
      setError(failure instanceof Error && failure.message === "当前选择的策略已不可用，请重新选择后再保存。"
        ? failure.message
        : describeError(failure));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card title="自动提炼设置" description="每天按助手分析有效对话，自动生成并发布技能，直接绑定到对应助手；已有同类自动技能会原地优化并保留历史。群聊共享会话不参与个人技能提炼。这里决定是否分析你的对话，以及由哪个已发布的模型策略执行提炼。">
      {settings.loading && !settings.data ? (
        <LoadingState />
      ) : settings.error ? (
        <ErrorBanner message={settings.error} onRetry={settings.reload} />
      ) : (
        <div className="flex flex-col gap-4">
          <label className="flex items-start gap-3 rounded-lg border border-line bg-paper-deep/40 p-3">
            <input
              type="checkbox"
              checked={enabled}
              onChange={(event) => setEnabled(event.target.checked)}
              className="mt-0.5 size-4 accent-[var(--color-accent)]"
            />
            <span className="min-w-0">
              <span className="block text-sm font-medium text-ink">开启每日技能自动提炼</span>
              <span className="mt-0.5 block text-xs leading-5 text-muted">关闭后系统不再分析你的对话，已生成的草稿仍会保留。</span>
            </span>
          </label>
          <Field label="提炼模型策略" hint="只能选择已发布的策略；留空时跟随管理员配置的平台默认策略。">
            <Select value={policyID} onChange={(event) => setPolicyID(event.target.value)}>
              <option value="">跟随平台默认策略</option>
              {publishedPolicies.map((policy) => (
                <option key={policy.id} value={policy.id}>{policy.name} · v{policy.version}</option>
              ))}
              {staleSelection ? (
                <option value={policyID}>当前策略（已失效）</option>
              ) : null}
            </Select>
          </Field>
          {policies.error ? <ErrorBanner message={policies.error} onRetry={policies.reload} /> : null}
          {!policies.loading && publishedPolicies.length === 0 ? (
            <Callout tone="warning" title="暂无可选策略">
              目前没有已发布的模型策略。可以跟随平台默认策略；如需指定策略，请联系管理员在“平台设置 · 选模规则”中发布。
            </Callout>
          ) : null}
          {staleSelection ? <Callout tone="warning">当前选择的策略已不可用，请重新选择后再保存。</Callout> : null}
          {error ? <ErrorBanner message={error} /> : null}
          <div className="flex flex-wrap items-center gap-3">
            <Button loading={busy} disabled={!initialized || busy} onClick={() => void save()}>保存设置</Button>
            <span className="text-xs text-muted">{savedAt ? `上次保存于 ${savedAt}` : "尚未保存过设置"}</span>
          </div>
        </div>
      )}
    </Card>
  );
}

// synthesisOutcomeLabels 把提炼结果原因翻译成用户语言。
const synthesisOutcomeLabels: Record<string, string> = {
  created: "已生成技能",
  optimized: "已优化已有技能",
  not_worthwhile: "没有值得沉淀的技能",
  low_confidence: "置信度不足",
  no_activity: "没有有效对话",
  no_new_messages: "上次提炼后没有新对话",
  duplicate_skill: "已有同类技能",
  invalid_candidate: "格式校验未通过",
  agent_missing: "会话助手已不存在",
  daily_limit_reached: "达到每日上限",
  disabled: "你已关闭自动提炼",
  model_unavailable: "提炼模型不可用",
  extraction_failed: "模型提炼失败",
  create_failed: "技能保存失败",
  settings_unavailable: "设置读取失败",
  sessions_unavailable: "会话读取失败",
  interrupted: "执行中断",
  panic: "执行异常",
  history: "历史执行记录",
};

// SynthesisRunsCard 展示每日自动提炼的执行记录与结果原因。
function SynthesisRunsCard() {
  const runs = useRemote(() => requestJSON<{ items: UserSkillSynthesisRun[] }>("/api/v1/user-skill-synthesis/runs?limit=90"), []);
  const settings = useRemote(() => requestJSON<UserSkillSynthesisSettings>("/api/v1/user-skill-synthesis/settings"), []);
  const toast = useToast();
  const [triggering, setTriggering] = useState(false);
  const enabled = settings.data?.enabled ?? false;
  const items = runs.data?.items ?? [];
  const paging = useClientPage(items, 10);

  // triggerSynthesis 立即入队一次提炼；同一分钟内的重复点击由后端去重。
  const triggerSynthesis = async () => {
    setTriggering(true);
    try {
      await mutate("/api/v1/user-skill-synthesis/trigger", "POST", {}, crypto.randomUUID());
      toast.show("已触发提炼，后台执行中；稍后会刷新记录", "success");
      window.setTimeout(() => {
        paging.reset();
        runs.reload();
      }, 2500);
    } catch (failure) {
      toast.show(describeError(failure), "danger");
    } finally {
      setTriggering(false);
    }
  };

  return (
    <Card title="自动提炼记录" description="每天最多一条记录；只分析上次提炼后的新对话，重试会更新当天结果。可分页查看最近 90 天记录。">
      <div className="mb-4 flex flex-wrap items-center justify-between gap-2 border-b border-line pb-3">
        <p className="text-xs text-muted">
          {enabled ? "开启状态下可随时手动执行一次，只处理上次提炼后的新对话。" : "开启自动提炼后才可以手动触发。"}
        </p>
        <Button size="sm" variant="secondary" loading={triggering} disabled={!enabled || triggering} onClick={() => void triggerSynthesis()}>
          立即提炼一次
        </Button>
      </div>
      {runs.loading && !runs.data ? (
        <LoadingState />
      ) : runs.error ? (
        <ErrorBanner message={runs.error} onRetry={runs.reload} />
      ) : items.length === 0 ? (
        <EmptyState title="还没有执行记录" description="开启自动提炼后系统每天执行一次；升级前的历史任务也会补录执行事实。" />
      ) : (
        <ul className="divide-y divide-line">
          {paging.pageItems.map((run) => (
            <li key={run.id} className="flex flex-col gap-1 py-3 first:pt-0 last:pb-0">
              <div className="flex flex-wrap items-center gap-2">
                <span className="text-sm font-medium text-ink">{run.date}</span>
                <Badge tone={statusTone(run.status)}>{statusLabel(run.status)}</Badge>
                <span className="text-xs text-ink-soft">{synthesisOutcomeLabels[run.outcome] ?? run.outcome}</span>
                {run.attempts > 1 ? <span className="text-xs text-muted">当天第 {run.attempts} 次执行</span> : null}
              </div>
              <p className="text-xs text-muted">
                扫描 {run.sessions_scanned} 个会话 · 使用 {run.sessions_used} 个会话 / {run.messages_used} 条消息
                {run.candidate_name ? ` · 候选「${run.candidate_name}」` : ""}
                {run.confidence > 0 ? ` · 置信度 ${run.confidence.toFixed(2)}` : ""}
              </p>
              {run.detail ? <p className="text-xs leading-5 text-muted">{run.detail}</p> : null}
              {run.results?.length ? (
                <div className="flex flex-wrap gap-1.5">
                  {run.results.map((result, index) => (
                    <span key={`${run.id}-${index}`} className="rounded border border-line bg-paper-deep px-2 py-0.5 text-[11px] text-ink-soft">
                      {result.agent_name || "通用"} · {synthesisOutcomeLabels[result.outcome] ?? result.outcome}
                      {result.skill_name ? ` · ${result.skill_name}` : ""}
                    </span>
                  ))}
                </div>
              ) : null}
              <p className="text-[11px] text-muted">
                {formatDateTime(run.started_at)}
                {run.finished_at ? ` → ${formatDateTime(run.finished_at)}` : ""}
              </p>
            </li>
          ))}
        </ul>
      )}
      {items.length > 0 ? (
        <Pager page={paging.page} totalPages={paging.totalPages} total={items.length} busy={runs.loading} onChange={paging.setPage} />
      ) : null}
    </Card>
  );
}

// UserSkillHistoryDrawer 展示技能被原地优化前的历史快照，并支持恢复。
function UserSkillHistoryDrawer({ skill, onClose, onRestored }: { skill: UserSkill; onClose: () => void; onRestored: () => void }) {
  const history = useRemote(
    () => requestJSON<{ items: UserSkillRevision[] }>(`/api/v1/user-skills/${skill.id}/revisions?limit=20`),
    [skill.id],
  );
  const toast = useToast();
  const [pending, setPending] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const revisions = history.data?.items ?? [];
  const restore = async (revision: UserSkillRevision) => {
    setBusy(true);
    try {
      await mutate<UserSkill>(`/api/v1/user-skills/${skill.id}/revisions/${revision.id}/restore`, "POST", undefined, crypto.randomUUID());
      toast.show("已恢复历史版本", "success");
      setPending(null);
      history.reload();
      onRestored();
    } catch (failure) {
      toast.show(describeError(failure), "danger");
    } finally {
      setBusy(false);
    }
  };
  return (
    <FormDrawer
      title={`${skill.name} · 历史版本`}
      description="每次原地优化前保存的内容快照，最近的在前。恢复后当前内容也会保留为一条历史。"
      onSubmit={(event) => event.preventDefault()}
      onCancel={onClose}
    >
      <div className="sm:col-span-2 flex flex-col gap-3">
        {history.error ? <ErrorBanner message={history.error} onRetry={history.reload} /> : null}
        {history.loading && !history.data ? (
          <LoadingState />
        ) : revisions.length === 0 ? (
          <EmptyState title="还没有历史版本" description="技能首次被自动优化后会在这里保留旧内容。" />
        ) : (
          revisions.map((revision) => (
            <article key={revision.id} className="rounded-lg border border-line bg-paper-deep/40 p-3">
              <div className="flex flex-wrap items-center gap-2 text-xs text-muted">
                <Badge tone="neutral">v{revision.version}</Badge>
                <span>{formatDateTime(revision.created_at)}</span>
                <span>置信度 {Math.round(revision.confidence * 100)}%</span>
                <div className="ml-auto flex items-center gap-2">
                  {pending === revision.id ? (
                    <>
                      <Button size="sm" variant="secondary" disabled={busy} onClick={() => setPending(null)}>取消</Button>
                      <Button size="sm" loading={busy} onClick={() => void restore(revision)}>确认恢复</Button>
                    </>
                  ) : (
                    <Button size="sm" variant="secondary" onClick={() => setPending(revision.id)}>恢复此版本</Button>
                  )}
                </div>
              </div>
              <p className="mt-2 text-sm text-ink">{revision.description || "无描述"}</p>
              <pre className="mt-2 max-h-48 overflow-auto whitespace-pre-wrap break-words rounded-md bg-paper-raised p-2 text-[11px] leading-5 text-ink-soft">
                {revision.prompt_template}
              </pre>
            </article>
          ))
        )}
      </div>
    </FormDrawer>
  );
}

export default function UserSkillsPage() {
  const skills = useRemote(() => requestJSON<UserSkillList>("/api/v1/user-skills?limit=200"), []);
  const toast = useToast();
  const [editing, setEditing] = useState<UserSkill | null | "new">(null);
  const [deleting, setDeleting] = useState<UserSkill | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [busyID, setBusyID] = useState<string | null>(null);
  const [bundleSkill, setBundleSkill] = useState<UserSkill | null>(null);
  const [historySkill, setHistorySkill] = useState<UserSkill | null>(null);
  const [importing, setImporting] = useState(false);
  const importRef = useRef<HTMLInputElement | null>(null);
  const skillItems = skills.data?.items ?? [];
  const paging = useClientPage(skillItems, 10);

  // importBundle 上传标准 SKILL 包 zip，后端保留完整目录树与脚本。
  const importBundle = async (file: File) => {
    setImporting(true);
    setActionError(null);
    try {
      const created = await uploadSkillPackage<UserSkill>("/api/v1/user-skills/import", file);
      toast.show(`已导入技能「${created.name}」，确认后可发布`, "success");
      skills.reload();
    } catch (failure) {
      setActionError(describeError(failure));
    } finally {
      setImporting(false);
      if (importRef.current) {
        importRef.current.value = "";
      }
    }
  };

  // runAction 统一串行动作状态，避免同一行重复提交。
  const runAction = async (skill: UserSkill, action: "publish" | "disable", successMessage: string) => {
    setBusyID(skill.id);
    setActionError(null);
    try {
      await mutate<UserSkill>(`/api/v1/user-skills/${skill.id}/${action}`, "POST", undefined, crypto.randomUUID());
      toast.show(successMessage, "success");
      skills.reload();
    } catch (failure) {
      setActionError(describeError(failure));
    } finally {
      setBusyID(null);
    }
  };

  const remove = async () => {
    if (!deleting) {
      return;
    }
    setBusyID(deleting.id);
    setActionError(null);
    try {
      await mutate<void>(`/api/v1/user-skills/${deleting.id}`, "DELETE");
      setDeleting(null);
      toast.show("技能已删除", "success");
      skills.reload();
    } catch (failure) {
      setActionError(describeError(failure));
    } finally {
      setBusyID(null);
    }
  };

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        eyebrow="工作区 · 能力"
        title="我的技能"
        description="这里是你私有的技能库。开启每日技能提炼后，系统会按助手从有效对话中生成并发布技能，直接绑定到对应助手；同类技能会原地优化并保留历史。也可以手动创建或导入标准 SKILL 包，发布后即可在助手中勾选使用。"
        actions={
          <>
            <input
              ref={importRef}
              type="file"
              accept=".zip,application/zip"
              className="hidden"
              onChange={(event) => {
                const file = event.target.files?.[0];
                if (file) {
                  void importBundle(file);
                }
              }}
            />
            <Button variant="secondary" loading={importing} onClick={() => importRef.current?.click()}>导入技能包</Button>
            <Button onClick={() => setEditing("new")}>新建技能</Button>
          </>
        }
      />
      {skills.error ? <ErrorBanner message={skills.error} onRetry={skills.reload} /> : null}
      {actionError ? <ErrorBanner message={actionError} /> : null}
      <SynthesisSettingsCard />
      <SynthesisRunsCard />
      {editing ? (
        <UserSkillForm
          initial={editing === "new" ? null : editing}
          onCancel={() => setEditing(null)}
          onSaved={() => { setEditing(null); skills.reload(); }}
        />
      ) : null}
      <Card title="技能列表" description={`共 ${skillItems.length} 个技能`}>
        {skills.loading && !skills.data ? (
          <LoadingState />
        ) : skillItems.length > 0 ? (
          <div className="flex flex-col gap-3">
            {paging.pageItems.map((skill) => {
              const busy = busyID === skill.id;
              return (
                <article key={skill.id} className="rounded-lg border border-line bg-paper-deep/40 p-4">
                  <div className="flex flex-wrap items-start justify-between gap-3">
                    <div className="min-w-0">
                      <div className="flex flex-wrap items-center gap-2">
                        <h3 className="text-sm font-semibold text-ink">{skill.name}</h3>
                        <Badge tone="neutral">v{skill.version}</Badge>
                        <Badge tone={statusTone(skill.status)}>{statusLabel(skill.status)}</Badge>
                        <Badge tone={skill.origin === "auto" ? "info" : "neutral"}>{originLabel(skill.origin)}</Badge>
                        {skill.user_edited_at ? <Badge tone="warning">已手动修改</Badge> : null}
                        {skill.origin === "auto" ? <Badge tone="neutral">置信度 {Math.round(skill.confidence * 100)}%</Badge> : null}
                        {skill.bundle ? <Badge tone="info">技能包 · {skill.bundle.file_count} 文件</Badge> : null}
                        {skill.bundle?.has_scripts ? <Badge tone={skill.bundle.execution_enabled ? "warning" : "neutral"}>{skill.bundle.execution_enabled ? "脚本可执行" : "含脚本"}</Badge> : null}
                      </div>
                      <p className="mt-1 text-xs text-muted">{skill.description || "无描述"}</p>
                      <p className="mt-1 text-[11px] text-muted">更新于 {formatDateTime(skill.updated_at)}</p>
                    </div>
                    <div className="flex items-center gap-2">
                      {skill.bundle ? (
                        <Button size="sm" variant="secondary" onClick={() => setBundleSkill(skill)}>文件</Button>
                      ) : null}
                      {skill.origin === "auto" ? (
                        <Button size="sm" variant="ghost" onClick={() => setHistorySkill(skill)}>历史</Button>
                      ) : null}
                      {skill.status !== "published" ? (
                        <Button size="sm" variant="secondary" loading={busy} disabled={busyID !== null && !busy} onClick={() => void runAction(skill, "publish", "技能已发布")}>发布</Button>
                      ) : null}
                      {skill.status !== "disabled" ? (
                        <Button size="sm" variant="secondary" loading={busy} disabled={busyID !== null && !busy} onClick={() => void runAction(skill, "disable", "技能已停用")}>停用</Button>
                      ) : null}
                      <RowMenu name={skill.name}>
                        {skill.status === "auto_generated" ? (
                          <MenuItem icon="edit" onClick={() => setEditing(skill)}>编辑草稿</MenuItem>
                        ) : null}
                        <MenuItem icon="trash" danger onClick={() => { setDeleting(skill); setActionError(null); }}>删除</MenuItem>
                      </RowMenu>
                    </div>
                  </div>
                  <details className="mt-3 text-xs text-muted">
                    <summary className="cursor-pointer text-muted">查看技能说明</summary>
                    <pre className="mt-2 overflow-x-auto whitespace-pre-wrap rounded border border-line bg-paper-raised p-3 font-mono text-[11px] text-ink-soft">{skill.prompt_template}</pre>
                    {skill.source_session_ids.length > 0 ? (
                      <p className="mt-2 break-all text-[11px] text-muted">来源会话：{skill.source_session_ids.join("、")}</p>
                    ) : null}
                  </details>
                  {skill.bundle && bundleSkill?.id === skill.id ? (
                    <div className="mt-3">
                      <SkillBundlePanel
                        basePath="/api/v1/user-skills"
                        skillId={skill.id}
                        skillName={skill.name}
                        bundle={skill.bundle}
                        onClose={() => setBundleSkill(null)}
                        onExecutionChanged={() => skills.reload()}
                      />
                    </div>
                  ) : null}
                </article>
              );
            })}
          </div>
        ) : (
          <EmptyState
            title="还没有技能"
            description="开启每日技能提炼后，系统会按助手从有效对话中生成并发布技能，直接绑定到对应助手；也可以现在手动创建或导入标准 SKILL 包。"
            action={<Button size="sm" onClick={() => setEditing("new")}>新建技能</Button>}
          />
        )}
        <Pager page={paging.page} totalPages={paging.totalPages} total={skillItems.length} busy={skills.loading} onChange={paging.setPage} />
      </Card>
      {historySkill ? <UserSkillHistoryDrawer skill={historySkill} onClose={() => setHistorySkill(null)} onRestored={() => skills.reload()} /> : null}
      <ConfirmDialog
        open={deleting !== null}
        title={`删除技能 ${deleting?.name ?? ""}？`}
        description="删除后无法恢复；已绑定该技能的助手需要重新选择技能。"
        confirmLabel="确认删除"
        busy={busyID !== null}
        onCancel={() => setDeleting(null)}
        onConfirm={() => void remove()}
      />
    </div>
  );
}
