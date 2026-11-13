// 选模规则：用“主选 + 替补”的排班方式替代 JSON 配置，降低理解成本。
import { useState } from "react";
import { FormDrawer } from "../../components/FormDrawer";
import { useToast } from "../../components/Toast";
import { Badge, Button, Callout, Card, ConfirmDialog, EmptyState, ErrorBanner, Field, Input, LoadingState, MenuItem, PageHeader, Pager, RowMenu, Select } from "../../components/ui";
import { ApiError, mutate, requestJSON } from "../../lib/api";
import { describeError } from "../../lib/errors";
import { formatDateTime, statusLabel, statusTone } from "../../lib/format";
import { useClientPage, useRemote } from "../../lib/hooks";
import type { Model, ModelPolicy, Page, PolicyCandidate } from "../../lib/types";

interface PolicyFormState {
  name: string;
  minContextTokens: string;
  candidates: PolicyCandidate[];
}

const minimumContext = 1_000_000;
const emptyPolicy: PolicyFormState = { name: "", minContextTokens: String(minimumContext), candidates: [] };

function isEligibleModel(model: Model, minContext: number): boolean {
  return model.kind === "chat" && model.status === "enabled" && model.context_window_tokens >= minContext
    && Boolean(model.verified_at && model.verification_source?.trim());
}

function candidateRegion(candidate: PolicyCandidate): string | null {
  const region = candidate.constraints?.data_region;
  return region == null || typeof region === "string" ? (region ?? "").trim() : null;
}

function withRegion(candidate: PolicyCandidate, region: string): PolicyCandidate {
  const constraints = { ...candidate.constraints };
  if (region) constraints.data_region = region;
  else delete constraints.data_region;
  return { ...candidate, constraints };
}

// slotLabel 把候选位置翻译成主选/替补，避免用户理解“优先级数字”。
function slotLabel(index: number): string {
  if (index === 0) {
    return "主选";
  }
  return `替补 ${index}`;
}

// describePolicyDeleteError 把删除冲突翻译成可执行的下一步。
function describePolicyDeleteError(failure: unknown): string {
  if (failure instanceof ApiError && failure.code === "resource_conflict") {
    return "这条规则正被助手引用，暂时不能删除。请先把引用它的助手切换到其他规则，再回来删除。";
  }
  return describeError(failure);
}

function PolicyForm({ initial, models, onSaved, onCancel }: { initial: ModelPolicy | null; models: Model[]; onSaved: () => void; onCancel: () => void }) {
  const initialForm: PolicyFormState =
    initial
      ? {
          name: initial.name,
          minContextTokens: String(initial.min_context_tokens),
          candidates: initial.candidates,
        }
      : emptyPolicy;
  const [form, setForm] = useState(initialForm);
  const toast = useToast();
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [addID, setAddID] = useState("");
  const [advanced, setAdvanced] = useState(() => Boolean(initial?.candidates.some((candidate) => candidateRegion(candidate) !== "")));
  const hasLegacyRules = Boolean(Object.keys(initial?.rules ?? {}).length);

  const modelByID = new Map(models.map((model) => [model.id, model]));
  const minContext = Number(form.minContextTokens);
  const invalidMinContext = !Number.isSafeInteger(minContext) || minContext < minimumContext;
  const eligibleModels = models
    .filter((model) => isEligibleModel(model, minContext))
    .filter((model) => !form.candidates.some((candidate) => candidate.model_id === model.id));
  const pendingModel = eligibleModels.find((model) => model.id === addID);
  const invalidRegion = form.candidates.some((candidate) => {
    const region = candidateRegion(candidate);
    const model = modelByID.get(candidate.model_id);
    return region === null || (region !== "" && Boolean(model) && model?.data_region !== region);
  });

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
      const body = {
        name: form.name.trim(),
        min_context_tokens: minContext,
        rules: initial?.rules ?? {},
        candidates: form.candidates.map(({ model_id, constraints }, index) => ({ model_id, priority: index + 1, constraints })),
      };
      if (initial) {
        await mutate<ModelPolicy>(`/api/v1/model-policies/${initial.id}`, "PUT", body, crypto.randomUUID());
      } else {
        await mutate<ModelPolicy>("/api/v1/model-policies", "POST", body, crypto.randomUUID());
      }
      toast.show(initial ? "修改已保存" : "已创建规则", "success");
      onSaved();
    } catch (submitError) {
      setError(describeError(submitError));
    } finally {
      setBusy(false);
    }
  };

  const summary = form.candidates.length === 0
    ? "还没有选择模型。至少加入一个主选模型才能创建规则。"
    : `优先使用「${modelByID.get(form.candidates[0].model_id)?.remote_model_name ?? "主选模型"}」${
        form.candidates.length > 1
          ? `；不可用时依次切换到 ${form.candidates.slice(1).map(({ model_id }) => `「${modelByID.get(model_id)?.remote_model_name ?? model_id}」`).join("、")}`
          : ""
      }。`;

  return (
    <FormDrawer
      title={initial ? `编辑「${initial.name}」` : "新建选模规则"}
      description="先排主选，再按顺序添加替补。系统遇到失败、超时或熔断时会自动切换到下一个。"
      submitLabel={initial ? "保存修改" : "创建规则"}
      busy={busy}
      dirty={JSON.stringify(form) !== JSON.stringify(initialForm)}
      error={error}
      bodyClassName="grid-cols-1"
      submitDisabled={form.candidates.length === 0 || invalidMinContext}
      onSubmit={submit}
      onCancel={onCancel}
    >
      <Field label="规则名称" hint="例如“日常任务”“高精度长文档”。给助手选择时你会看到这个名字。">
        <Input value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} required maxLength={128} autoFocus placeholder="给规则起个容易记住的名字" />
      </Field>

      <div className="rounded-[3px] border border-line bg-paper-deep/30 p-4">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div>
            <p className="text-sm font-medium text-ink">模型出场顺序</p>
            <p className="mt-1 text-xs leading-5 text-muted">第 1 个是主选，其余依序替补。用「上移」「下移」调整顺序。</p>
          </div>
          <Badge tone="neutral">{form.candidates.length} 个候选</Badge>
        </div>

        <div className="mt-3 flex flex-wrap items-center gap-2">
          <Select className="min-w-56 flex-1" value={pendingModel?.id ?? ""} onChange={(event) => setAddID(event.target.value)} aria-label="选择要加入的模型">
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
            disabled={!pendingModel || form.candidates.length >= 16}
            onClick={() => {
              if (!pendingModel || form.candidates.length >= 16) return;
              setForm({ ...form, candidates: [...form.candidates, { model_id: pendingModel.id }] });
              setAddID("");
            }}
          >
            加入名单
          </Button>
        </div>
        {form.candidates.length >= 16 ? (
          <p className="mt-2 text-xs text-muted">最多可添加 16 个候选模型。</p>
        ) : eligibleModels.length === 0 ? (
          <p className="mt-2 text-xs leading-5 text-muted">
            请在「可用模型」登记并启用已核验的生成模型，模型须达到当前最低上下文要求。
          </p>
        ) : null}

        {form.candidates.length === 0 ? (
          <div className="mt-3 rounded-[3px] border border-dashed border-line-strong px-4 py-6 text-center text-xs text-muted">
            还没有候选模型。先选一个主选模型。
          </div>
        ) : (
          <ol className="mt-3 flex flex-col gap-2">
            {form.candidates.map(({ model_id: modelID }, index) => {
              const model = modelByID.get(modelID);
              const eligible = model && isEligibleModel(model, minContext);
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

      <details open={advanced} onToggle={(event) => setAdvanced(event.currentTarget.open)} className="rounded-lg border border-line bg-paper-raised px-4 py-3">
        <summary className="cursor-pointer rounded text-sm font-medium text-ink">高级选项</summary>
        <div className="mt-4 space-y-4">
          <Field label="最低上下文（Token）" hint="每个候选模型都必须达到此值，最低为 1,000,000。" error={invalidMinContext ? "请输入不小于 1,000,000 的整数。" : null}>
            <Input type="number" min={minimumContext} value={form.minContextTokens} onChange={(event) => setForm({ ...form, minContextTokens: event.target.value })} required />
          </Field>
          <section aria-label="候选模型数据区域" className="space-y-3 border-t border-line pt-4">
            <div>
              <h3 className="text-sm font-semibold">候选模型数据区域</h3>
              <p className="mt-1 text-xs leading-5 text-muted">默认不限。选择区域后，模型登记的数据区域必须与之匹配，才能发布和执行。</p>
            </div>
            {!form.candidates.length ? <p className="text-xs text-muted">加入候选模型后，可为每个候选设置区域。</p> : null}
            {form.candidates.map((candidate, index) => {
              const model = modelByID.get(candidate.model_id);
              const region = candidateRegion(candidate);
              const modelRegion = model?.data_region;
              const mismatch = region === null || (region !== "" && Boolean(model) && region !== modelRegion);
              return (
                <Field
                  key={candidate.model_id}
                  label={`${slotLabel(index)} · ${model?.remote_model_name ?? candidate.model_id}`}
                  error={mismatch ? "现有区域约束与模型登记信息不匹配，请重新选择。" : null}
                >
                  <Select value={mismatch ? "__invalid__" : region ?? ""} onChange={(event) => {
                    const nextRegion = event.target.value;
                    setForm((current) => ({ ...current, candidates: current.candidates.map((item) =>
                      item.model_id === candidate.model_id ? withRegion(item, nextRegion) : item,
                    ) }));
                  }}>
                    {mismatch ? <option value="__invalid__" disabled>{region || "无效的区域设置"}</option> : null}
                    <option value="">不限区域</option>
                    {!model && region ? <option value={region}>仅限 {region}（现有约束）</option> : null}
                    {modelRegion ? <option value={modelRegion}>仅限 {modelRegion}</option> : null}
                  </Select>
                </Field>
              );
            })}
          </section>
          {hasLegacyRules ? (
            <details className="rounded-lg border border-line p-3">
              <summary className="cursor-pointer text-sm text-muted">历史附加规则</summary>
              <p className="mt-2 text-xs text-muted">以下 JSON 会随策略保存，当前路由不会读取这些字段。</p>
              <pre className="mt-2 max-h-40 overflow-auto rounded bg-paper p-2 font-mono text-xs">{JSON.stringify(initial?.rules, null, 2)}</pre>
            </details>
          ) : null}
        </div>
      </details>
      {invalidMinContext && !advanced ? (
        <p role="alert" className="text-sm text-rust">请在高级选项将最低上下文设为不小于 1,000,000 的整数。</p>
      ) : null}
      {invalidRegion ? <p className="text-sm text-ochre">可以保存草稿。发布前请修正高级选项中的数据区域约束。</p> : null}

    </FormDrawer>
  );
}

export default function PoliciesPage() {
  const models = useRemote(() => requestJSON<Page<Model>>("/api/v1/models"), []);
  const policies = useRemote(() => requestJSON<Page<ModelPolicy>>("/api/v1/model-policies"), []);
  const [editing, setEditing] = useState<ModelPolicy | null | "new">(null);
  const [viewing, setViewing] = useState<ModelPolicy | null>(null);
  const [deleting, setDeleting] = useState<ModelPolicy | null>(null);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [batchOpen, setBatchOpen] = useState(false);
  const [batchError, setBatchError] = useState<string | null>(null);

  const items = policies.data?.items ?? [];
  const paging = useClientPage(items, 10);

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
      setDeleteError(describePolicyDeleteError(failure));
    } finally {
      setBusy(false);
    }
  };

  const allSelected = paging.pageItems.length > 0 && paging.pageItems.every((policy) => selected.has(policy.id));
  const toggleSelected = (id: string) => {
    setSelected((current) => {
      const next = new Set(current);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });
  };
  const toggleAll = () => {
    const pageIDs = new Set(paging.pageItems.map((policy) => policy.id));
    setSelected((current) => {
      const next = new Set(current);
      if (paging.pageItems.every((policy) => next.has(policy.id))) {
        pageIDs.forEach((id) => next.delete(id));
      } else {
        pageIDs.forEach((id) => next.add(id));
      }
      return next;
    });
  };

  // 批量删除逐条调用现有接口，失败项保留在列表中并汇总原因。
  const removeSelected = async () => {
    const targets = items.filter((policy) => selected.has(policy.id));
    if (targets.length === 0) {
      return;
    }
    setBusy(true);
    setBatchError(null);
    const failures: string[] = [];
    for (const policy of targets) {
      try {
        await mutate<void>(`/api/v1/model-policies/${policy.id}`, "DELETE");
      } catch (failure) {
        failures.push(`${policy.name}：${describePolicyDeleteError(failure)}`);
      }
    }
    setBusy(false);
    setBatchOpen(false);
    setSelected(new Set());
    policies.reload();
    if (failures.length > 0) {
      setBatchError(`部分规则未能删除：${failures.join("；")}`);
    }
  };

  const modelNames = new Map((models.data?.items ?? []).map((model) => [model.id, model.remote_model_name]));
  const hasModels = (models.data?.items ?? []).some((model) => isEligibleModel(model, minimumContext));

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

      {viewing ? (
        <Card
          title={`规则详情：${viewing.name}`}
          description={`第 ${viewing.version} 版 · 创建于 ${formatDateTime(viewing.created_at)} · 更新于 ${formatDateTime(viewing.updated_at)}`}
          actions={<Button size="sm" variant="secondary" onClick={() => setViewing(null)}>关闭详情</Button>}
        >
          <div className="grid gap-4 md:grid-cols-2">
            <dl className="grid grid-cols-2 gap-2 text-xs">
              <div className="rounded-[3px] border border-line bg-paper-deep/40 px-3 py-2">
                <dt className="text-muted">状态</dt>
                <dd className="mt-1"><Badge tone={statusTone(viewing.status)}>{statusLabel(viewing.status)}</Badge></dd>
              </div>
              <div className="rounded-[3px] border border-line bg-paper-deep/40 px-3 py-2">
                <dt className="text-muted">最低上下文</dt>
                <dd className="mt-1 text-ink">{viewing.min_context_tokens.toLocaleString("zh-CN")} Token</dd>
              </div>
              <div className="rounded-[3px] border border-line bg-paper-deep/40 px-3 py-2">
                <dt className="text-muted">候选模型</dt>
                <dd className="mt-1 text-ink">{viewing.candidates.length} 个</dd>
              </div>
              <div className="rounded-[3px] border border-line bg-paper-deep/40 px-3 py-2">
                <dt className="text-muted">规则 ID</dt>
                <dd className="mt-1 break-all font-mono text-[11px] text-ink-soft">{viewing.id}</dd>
              </div>
            </dl>
            <div className="rounded-[3px] border border-line bg-paper-deep/40 p-3">
              <p className="text-xs font-medium text-ink">模型出场顺序</p>
              {viewing.candidates.length === 0 ? (
                <p className="mt-2 text-xs text-muted">还没有候选模型。</p>
              ) : (
                <ol className="mt-2 flex flex-col gap-1 text-xs">
                  {viewing.candidates.map((candidate, index) => (
                    <li key={candidate.model_id} className="flex items-center justify-between gap-2 rounded-[2px] bg-paper-raised px-2 py-1.5">
                      <span className="text-ink-soft">{slotLabel(index)}</span>
                      <span className="min-w-0 truncate text-ink">{modelNames.get(candidate.model_id) ?? candidate.model_id}</span>
                      <span className="shrink-0 text-muted">{candidateRegion(candidate) === null ? "无效区域" : candidateRegion(candidate) || "不限区域"}</span>
                    </li>
                  ))}
                </ol>
              )}
            </div>
          </div>
          {Object.keys(viewing.rules ?? {}).length ? (
            <details className="mt-4 rounded-lg border border-line p-3">
              <summary className="cursor-pointer text-xs font-medium text-ink">历史附加规则（当前路由不会读取）</summary>
              <pre className="mt-2 max-h-64 overflow-auto rounded bg-paper p-3 font-mono text-xs">{JSON.stringify(viewing.rules, null, 2)}</pre>
            </details>
          ) : null}
        </Card>
      ) : null}

      <Card
        title="已有规则"
        description={`状态为“已发布”的规则才能被助手选择。共 ${items.length} 条。`}
        actions={
          selected.size > 0 ? (
            <>
              <span className="text-xs text-muted">已选 {selected.size} 项</span>
              <Button size="sm" variant="danger" onClick={() => setBatchOpen(true)}>批量删除</Button>
            </>
          ) : null
        }
      >
        {policies.loading && !policies.data ? (
          <LoadingState />
        ) : policies.data && policies.data.items.length > 0 ? (
          <div className="flex flex-col gap-3">
            <div className="flex items-center gap-2 text-xs text-muted">
              <input type="checkbox" checked={allSelected} onChange={toggleAll} aria-label="全选当前页规则" />
              全选当前页
            </div>
            {paging.pageItems.map((policy) => (
              <article key={policy.id} className="rounded-[3px] border border-line bg-paper-raised p-4">
                <div className="flex flex-wrap items-start justify-between gap-3">
                  <div className="flex items-start gap-3">
                    <input
                      type="checkbox"
                      className="mt-1.5"
                      checked={selected.has(policy.id)}
                      onChange={() => toggleSelected(policy.id)}
                      aria-label={`选择规则 ${policy.name}`}
                    />
                    <div>
                      <div className="flex flex-wrap items-center gap-2">
                        <h2 className="text-lg text-ink">{policy.name}</h2>
                        <Badge tone="neutral">v{policy.version}</Badge>
                        <Badge tone={statusTone(policy.status)}>{statusLabel(policy.status)}</Badge>
                      </div>
                      <p className="mt-1 text-xs text-muted">最低上下文 {policy.min_context_tokens.toLocaleString("zh-CN")} Token · 更新于 {formatDateTime(policy.updated_at)}</p>
                    </div>
                  </div>
                  <RowMenu name={policy.name}>
                    <MenuItem onClick={() => setViewing(policy)}>查看详情</MenuItem>
                    {policy.status === "draft" ? (
                      <>
                        <MenuItem icon="edit" onClick={() => setEditing(policy)}>继续编辑</MenuItem>
                        <MenuItem disabled={busy} onClick={() => void runAction(() => mutate(`/api/v1/model-policies/${policy.id}/publish`, "POST", undefined, crypto.randomUUID()))}>发布</MenuItem>
                      </>
                    ) : (
                      <MenuItem disabled={busy} onClick={() => void runAction(() => mutate(`/api/v1/model-policies/${policy.id}/versions`, "POST", undefined, crypto.randomUUID()))}>创建新版本</MenuItem>
                    )}
                    {/* 删除入口对所有状态可见；是否允许删除由后端按引用关系判定。 */}
                    <MenuItem icon="trash" danger disabled={busy} onClick={() => { setDeleting(policy); setDeleteError(null); }}>删除</MenuItem>
                  </RowMenu>
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
            <Pager page={paging.page} totalPages={paging.totalPages} total={items.length} onChange={paging.setPage} />
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
      <ConfirmDialog
        open={batchOpen}
        title={`删除选中的 ${selected.size} 条规则？`}
        description="删除会逐条执行。被助手引用的规则会失败并列出原因，其余规则仍会正常删除。"
        confirmLabel="批量删除"
        busy={busy}
        onCancel={() => setBatchOpen(false)}
        onConfirm={() => void removeSelected()}
      >
        {batchError ? <p role="alert" className="text-xs text-rust">{batchError}</p> : null}
      </ConfirmDialog>
    </div>
  );
}
