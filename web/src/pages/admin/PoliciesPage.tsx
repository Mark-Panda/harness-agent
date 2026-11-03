// 选模规则：用“主选 + 替补”的排班方式替代 JSON 配置，降低理解成本。
import { useState } from "react";
import { Badge, Button, Callout, Card, ConfirmDialog, EmptyState, ErrorBanner, Field, Input, LoadingState, PageHeader, Select, Textarea } from "../../components/ui";
import { mutate, requestJSON } from "../../lib/api";
import { describeError } from "../../lib/errors";
import { formatDateTime, statusLabel, statusTone } from "../../lib/format";
import { useRemote } from "../../lib/hooks";
import type { Model, ModelPolicy, Page } from "../../lib/types";

interface PolicyFormState {
  name: string;
  minContextTokens: string;
  rules: string;
  candidates: string[];
}

const emptyPolicy: PolicyFormState = { name: "", minContextTokens: "1000000", rules: "{}", candidates: [] };

// slotLabel 把候选位置翻译成主选/替补，避免用户理解“优先级数字”。
function slotLabel(index: number): string {
  if (index === 0) {
    return "主选";
  }
  return `替补 ${index}`;
}

function PolicyForm({ initial, models, onSaved, onCancel }: { initial: ModelPolicy | null; models: Model[]; onSaved: () => void; onCancel: () => void }) {
  const [form, setForm] = useState<PolicyFormState>(
    initial
      ? {
          name: initial.name,
          minContextTokens: String(initial.min_context_tokens),
          rules: JSON.stringify(initial.rules ?? {}, null, 2),
          candidates: initial.candidates.map((candidate) => candidate.model_id),
        }
      : emptyPolicy,
  );
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [addID, setAddID] = useState("");
  const [advanced, setAdvanced] = useState(false);

  const modelByID = new Map(models.map((model) => [model.id, model]));
  const minContext = Number(form.minContextTokens || 0);
  const eligibleModels = models
    .filter((model) => model.kind === "chat" && model.context_window_tokens >= minContext && model.verified_at)
    .filter((model) => !form.candidates.includes(model.id));

  const move = (index: number, delta: number) => {
    const next = [...form.candidates];
    const target = index + delta;
    if (target < 0 || target >= next.length) {
      return;
    }
    [next[index], next[target]] = [next[target], next[index]];
    setForm({ ...form, candidates: next });
  };

  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      let parsedRules: Record<string, unknown>;
      try {
        parsedRules = JSON.parse(form.rules || "{}") as Record<string, unknown>;
      } catch {
        throw new Error("高级规则不是合法 JSON；如果不需要，请保持 {}。");
      }
      const body = {
        name: form.name.trim(),
        min_context_tokens: minContext,
        rules: parsedRules,
        candidates: form.candidates.map((modelID, index) => ({ model_id: modelID, priority: index + 1 })),
      };
      if (initial) {
        await mutate<ModelPolicy>(`/api/v1/model-policies/${initial.id}`, "PUT", body, crypto.randomUUID());
      } else {
        await mutate<ModelPolicy>("/api/v1/model-policies", "POST", body, crypto.randomUUID());
      }
      onSaved();
    } catch (submitError) {
      const message = submitError instanceof Error && submitError.message.includes("高级规则") ? submitError.message : describeError(submitError);
      setError(message);
    } finally {
      setBusy(false);
    }
  };

  const summary = form.candidates.length === 0
    ? "还没有选择模型。至少加入一个主选模型才能创建规则。"
    : `优先使用「${modelByID.get(form.candidates[0])?.remote_model_name ?? "主选模型"}」${
        form.candidates.length > 1
          ? `；不可用时依次切换到 ${form.candidates.slice(1).map((id) => `「${modelByID.get(id)?.remote_model_name ?? id}」`).join("、")}`
          : ""
      }。`;

  return (
    <Card
      title={initial ? `编辑「${initial.name}」` : "新建选模规则"}
      description="先排主选，再按顺序添加替补。系统遇到失败、超时或熔断时会自动切换到下一个。"
    >
      <form onSubmit={submit} className="flex flex-col gap-5">
        <Field label="规则名称" hint="例如“日常任务”“高精度长文档”。给助手选择时你会看到这个名字。">
          <Input value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} required maxLength={128} autoFocus placeholder="给规则起个容易记住的名字" />
        </Field>

        <div className="rounded-[3px] border border-line bg-paper-deep/30 p-4">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <div>
              <p className="text-sm font-medium text-ink">模型出场顺序</p>
              <p className="mt-1 text-xs leading-5 text-muted">第 1 个是主选，其余是替补。拖动不方便时用“上移/下移”调整。</p>
            </div>
            <Badge tone="neutral">{form.candidates.length} 个候选</Badge>
          </div>

          <div className="mt-3 flex flex-wrap items-center gap-2">
            <Select className="min-w-56 flex-1" value={addID} onChange={(event) => setAddID(event.target.value)} aria-label="选择要加入的模型">
              <option value="">选择一个模型加入名单</option>
              {eligibleModels.map((model) => (
                <option key={model.id} value={model.id}>
                  {model.remote_model_name}（上下文 {model.context_window_tokens.toLocaleString("zh-CN")}）
                </option>
              ))}
            </Select>
            <Button
              type="button"
              variant="secondary"
              disabled={!addID}
              onClick={() => {
                setForm({ ...form, candidates: [...form.candidates, addID] });
                setAddID("");
              }}
            >
              加入名单
            </Button>
          </div>
          {eligibleModels.length === 0 ? (
            <p className="mt-2 text-xs leading-5 text-muted">
              没有可加入的模型。请先到“可用模型”登记已核验的生成模型，或降低最低上下文要求。
            </p>
          ) : null}

          {form.candidates.length === 0 ? (
            <div className="mt-3 rounded-[3px] border border-dashed border-line-strong px-4 py-6 text-center text-xs text-muted">
              还没有候选模型。先选一个主选模型。
            </div>
          ) : (
            <ol className="mt-3 flex flex-col gap-2">
              {form.candidates.map((modelID, index) => {
                const model = modelByID.get(modelID);
                const eligible = model && model.kind === "chat" && model.context_window_tokens >= minContext && model.verified_at && model.status === "enabled";
                return (
                  <li key={modelID} className="flex flex-wrap items-center gap-3 rounded-[3px] border border-line bg-paper-raised px-3 py-2.5">
                    <span className="flex w-14 shrink-0 items-center justify-center rounded-[2px] border border-line-strong px-1 py-1 text-[11px] text-ink-soft">
                      {slotLabel(index)}
                    </span>
                    <div className="min-w-0 flex-1">
                      <p className="truncate text-sm font-medium text-ink">{model?.remote_model_name ?? modelID}</p>
                      <p className={eligible ? "text-[11px] text-muted" : "text-[11px] text-rust"}>
                        {model ? `上下文 ${model.context_window_tokens.toLocaleString("zh-CN")} · ${model.status === "enabled" ? "已启用" : "已停用"}` : "模型不存在"}
                        {!eligible ? " · 不满足当前规则" : ""}
                      </p>
                    </div>
                    <div className="flex gap-1">
                      <Button type="button" size="sm" variant="ghost" onClick={() => move(index, -1)} disabled={index === 0} aria-label="上移">
                        上移
                      </Button>
                      <Button type="button" size="sm" variant="ghost" onClick={() => move(index, 1)} disabled={index === form.candidates.length - 1} aria-label="下移">
                        下移
                      </Button>
                      <Button type="button" size="sm" variant="ghost" onClick={() => setForm({ ...form, candidates: form.candidates.filter((_, i) => i !== index) })}>
                        移除
                      </Button>
                    </div>
                  </li>
                );
              })}
            </ol>
          )}

          <p className="mt-3 rounded-[3px] border-l-4 border-jade bg-jade-soft/40 px-3 py-2 text-xs leading-5 text-jade">
            效果预览：{summary}
          </p>
        </div>

        <details open={advanced} onToggle={(event) => setAdvanced((event.target as HTMLDetailsElement).open)} className="rounded-[3px] border border-line bg-paper-raised px-4 py-3">
          <summary className="cursor-pointer text-sm font-medium text-ink">高级选项（大多数情况不用改）</summary>
          <div className="mt-4 grid gap-4 md:grid-cols-2">
            <Field label="最低上下文 Token" hint="生成模型必须达到这个长度。默认 1,000,000，调低会放宽准入。">
              <Input type="number" min={1} value={form.minContextTokens} onChange={(event) => setForm({ ...form, minContextTokens: event.target.value })} required />
            </Field>
            <Field label="附加规则 JSON（可选）" hint="用于数据区域等特殊约束。不确定就保持 {}。">
              <Textarea className="min-h-28 font-mono text-xs" value={form.rules} onChange={(event) => setForm({ ...form, rules: event.target.value })} spellCheck={false} />
            </Field>
          </div>
        </details>

        {error ? <Callout tone="danger">{error}</Callout> : null}

        <div className="flex flex-wrap justify-end gap-2">
          <Button type="button" variant="secondary" onClick={onCancel}>取消</Button>
          <Button type="submit" loading={busy} disabled={form.candidates.length === 0}>
            {initial ? "保存修改" : "创建规则"}
          </Button>
        </div>
      </form>
    </Card>
  );
}

export default function PoliciesPage() {
  const models = useRemote(() => requestJSON<Page<Model>>("/api/v1/models"), []);
  const policies = useRemote(() => requestJSON<Page<ModelPolicy>>("/api/v1/model-policies"), []);
  const [editing, setEditing] = useState<ModelPolicy | null | "new">(null);
  const [deleting, setDeleting] = useState<ModelPolicy | null>(null);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

  const runAction = async (action: () => Promise<unknown>) => {
    setBusy(true);
    setActionError(null);
    try {
      await action();
      policies.reload();
    } catch (failure) {
      setActionError(describeError(failure));
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    if (!deleting) {
      return;
    }
    setBusy(true);
    setDeleteError(null);
    try {
      await mutate<void>(`/api/v1/model-policies/${deleting.id}`, "DELETE");
      setDeleting(null);
      policies.reload();
    } catch (failure) {
      setDeleteError(describeError(failure));
    } finally {
      setBusy(false);
    }
  };

  const modelNames = new Map((models.data?.items ?? []).map((model) => [model.id, model.remote_model_name]));
  const hasModels = (models.data?.items ?? []).some((model) => model.kind === "chat" && model.verified_at);

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        eyebrow="平台设置"
        title="选模规则"
        description="把模型想成值班表：先安排主选，再排替补。助手遇到模型失败或熔断时，会按这个顺序自动切换。大多数情况用默认值即可；上下文下限与附加规则在表单的“高级选项”里调整。"
        actions={<Button onClick={() => setEditing("new")} disabled={!hasModels}>新建规则</Button>}
      />

      {!hasModels ? (
        <Callout tone="warning" title="先登记模型">
          还没有可用的生成模型。请先到“平台设置 → 可用模型”登记并核验至少一个模型，再回来创建规则。
        </Callout>
      ) : null}

      {models.error ? <ErrorBanner message={models.error} onRetry={models.reload} /> : null}
      {policies.error ? <ErrorBanner message={policies.error} onRetry={policies.reload} /> : null}
      {actionError ? <ErrorBanner message={actionError} /> : null}

      {editing ? (
        <PolicyForm
          initial={editing === "new" ? null : editing}
          models={models.data?.items ?? []}
          onCancel={() => setEditing(null)}
          onSaved={() => { setEditing(null); policies.reload(); }}
        />
      ) : null}

      <Card title="已有规则" description="状态为“已发布”的规则才能被助手选择。">
        {policies.loading ? (
          <LoadingState />
        ) : policies.data && policies.data.items.length > 0 ? (
          <div className="flex flex-col gap-3">
            {policies.data.items.map((policy) => (
              <article key={policy.id} className="rounded-[3px] border border-line bg-paper-raised p-4">
                <div className="flex flex-wrap items-start justify-between gap-3">
                  <div>
                    <div className="flex flex-wrap items-center gap-2">
                      <h2 className="text-lg text-ink">{policy.name}</h2>
                      <Badge tone="neutral">v{policy.version}</Badge>
                      <Badge tone={statusTone(policy.status)}>{statusLabel(policy.status)}</Badge>
                    </div>
                    <p className="mt-1 text-xs text-muted">最低上下文 {policy.min_context_tokens.toLocaleString("zh-CN")} Token · 更新于 {formatDateTime(policy.updated_at)}</p>
                  </div>
                  <div className="flex flex-wrap gap-2">
                    {policy.status === "draft" ? (
                      <>
                        <Button size="sm" variant="secondary" onClick={() => setEditing(policy)}>继续编辑</Button>
                        <Button size="sm" loading={busy} onClick={() => void runAction(() => mutate(`/api/v1/model-policies/${policy.id}/publish`, "POST", undefined, crypto.randomUUID()))}>发布</Button>
                      </>
                    ) : (
                      <Button size="sm" variant="secondary" loading={busy} onClick={() => void runAction(() => mutate(`/api/v1/model-policies/${policy.id}/versions`, "POST", undefined, crypto.randomUUID()))}>创建新版本</Button>
                    )}
                    {/* 删除入口对所有状态可见；是否允许删除由后端按引用关系判定。 */}
                    <Button size="sm" variant="danger" onClick={() => { setDeleting(policy); setDeleteError(null); }}>删除</Button>
                  </div>
                </div>

                <div className="mt-3 flex flex-wrap items-center gap-2">
                  {policy.candidates.length === 0 ? (
                    <span className="text-xs text-muted">还没有候选模型</span>
                  ) : (
                    policy.candidates.map((candidate, index) => (
                      <span key={candidate.model_id} className="inline-flex items-center gap-2 rounded-[2px] border border-line-strong bg-paper-deep/50 px-2 py-1 text-xs text-ink-soft">
                        <span className="font-display text-accent">{slotLabel(index)}</span>
                        {modelNames.get(candidate.model_id) ?? candidate.model_id.slice(0, 8)}
                      </span>
                    ))
                  )}
                </div>
                {policy.candidates.length > 0 ? (
                  <p className="mt-2 text-xs leading-5 text-muted">
                    优先 {modelNames.get(policy.candidates[0].model_id) ?? "主选模型"}
                    {policy.candidates.length > 1 ? `，失败时依次切换到 ${policy.candidates.slice(1).map((candidate) => modelNames.get(candidate.model_id) ?? "替补模型").join("、")}` : ""}。
                  </p>
                ) : null}
              </article>
            ))}
          </div>
        ) : (
          <EmptyState
            title="还没有选模规则"
            description="创建一套规则，安排主选与替补模型。助手创建时会引用这套规则。"
            action={<Button size="sm" onClick={() => setEditing("new")} disabled={!hasModels}>新建规则</Button>}
          />
        )}
      </Card>

      <ConfirmDialog
        open={deleting !== null}
        title={`删除规则「${deleting?.name ?? ""}」？`}
        description="已被助手引用的规则不能删除。需要调整时，请先让助手使用新版本。"
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
