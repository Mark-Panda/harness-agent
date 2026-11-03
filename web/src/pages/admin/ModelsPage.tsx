// 模型目录：展示上下文窗口核验、能力与价格，生成模型强制 1M+ 上下文。
import { useState } from "react";
import { Badge, Button, Card, ConfirmDialog, EmptyState, ErrorBanner, Field, Input, LoadingState, PageHeader, Select } from "../../components/ui";
import { mutate, requestJSON } from "../../lib/api";
import { describeError } from "../../lib/errors";
import { formatDateTime, formatNumber, statusLabel, statusTone } from "../../lib/format";
import { useRemote } from "../../lib/hooks";
import type { Model, ModelHealth, ModelKind, Page, Provider } from "../../lib/types";

interface ModelFormState {
  providerId: string;
  remoteModelName: string;
  kind: ModelKind;
  contextWindowTokens: string;
  maxOutputTokens: string;
  inputPrice: string;
  outputPrice: string;
  dataRegion: string;
  verificationSource: string;
  verifiedAt: string;
  status: "enabled" | "disabled";
  streaming: boolean;
  tools: boolean;
  structuredOutput: boolean;
}

const emptyModel: ModelFormState = {
  providerId: "",
  remoteModelName: "",
  kind: "chat",
  contextWindowTokens: "1000000",
  maxOutputTokens: "8192",
  inputPrice: "0",
  outputPrice: "0",
  dataRegion: "cn",
  verificationSource: "",
  verifiedAt: "",
  status: "enabled",
  streaming: true,
  tools: true,
  structuredOutput: false,
};

// toLocalInput 将 ISO 时间转换为 datetime-local 输入值。
function toLocalInput(value: string | null): string {
  if (!value) {
    return "";
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return "";
  }
  return date.toISOString().slice(0, 16);
}

function ModelForm({ initial, providers, onSaved, onCancel }: { initial: Model | null; providers: Provider[]; onSaved: () => void; onCancel: () => void }) {
  const [form, setForm] = useState<ModelFormState>(
    initial
      ? {
          providerId: initial.provider_id,
          remoteModelName: initial.remote_model_name,
          kind: initial.kind,
          contextWindowTokens: String(initial.context_window_tokens),
          maxOutputTokens: String(initial.max_output_tokens),
          inputPrice: String(initial.input_price_per_million),
          outputPrice: String(initial.output_price_per_million),
          dataRegion: initial.data_region,
          verificationSource: initial.verification_source,
          verifiedAt: toLocalInput(initial.verified_at),
          status: initial.status,
          streaming: Boolean(initial.capabilities?.streaming),
          tools: Boolean(initial.capabilities?.tools),
          structuredOutput: Boolean(initial.capabilities?.structured_output),
        }
      : emptyModel,
  );
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const contextTokens = Number(form.contextWindowTokens || 0);
  const contextWarning = form.kind === "chat" && contextTokens < 1_000_000;
  const verificationWarning = form.kind === "chat" && (!form.verifiedAt || !form.verificationSource.trim());

  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const body = {
        provider_id: form.providerId,
        remote_model_name: form.remoteModelName.trim(),
        kind: form.kind,
        context_window_tokens: contextTokens,
        max_output_tokens: Number(form.maxOutputTokens || 0),
        capabilities: { streaming: form.streaming, tools: form.tools, structured_output: form.structuredOutput },
        input_price_per_million: Number(form.inputPrice || 0),
        output_price_per_million: Number(form.outputPrice || 0),
        data_region: form.dataRegion.trim(),
        verification_source: form.verificationSource.trim(),
        verified_at: form.verifiedAt ? new Date(form.verifiedAt).toISOString() : null,
        status: form.status,
      };
      if (initial) {
        await mutate<Model>(`/api/v1/models/${initial.id}`, "PUT", body, crypto.randomUUID());
      } else {
        await mutate<Model>("/api/v1/models", "POST", body, crypto.randomUUID());
      }
      onSaved();
    } catch (submitError) {
      setError(describeError(submitError));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card title={initial ? `编辑模型：${initial.remote_model_name}` : "登记模型"} description="登记后，模型才能被选模规则引用。生成模型需要 1,000,000 以上上下文，并记录核验来源。">
      <form onSubmit={submit} className="grid gap-4 md:grid-cols-2">
        <Field label="来自哪个服务" hint="先接入模型服务，这里才能选择。">
          <Select value={form.providerId} onChange={(event) => setForm({ ...form, providerId: event.target.value })} required>
            <option value="">选择模型服务</option>
            {providers.map((provider) => (
              <option key={provider.id} value={provider.id}>{provider.name}</option>
            ))}
          </Select>
        </Field>
        <Field label="模型名称" hint="必须与服务商控制台里的模型 ID 完全一致。">
          <Input value={form.remoteModelName} onChange={(event) => setForm({ ...form, remoteModelName: event.target.value })} required placeholder="例如：deepseek-chat" />
        </Field>
        <Field label="用它来做什么" hint="大多数助手只用“生成”；向量与重排用于记忆检索。">
          <Select value={form.kind} onChange={(event) => setForm({ ...form, kind: event.target.value as ModelKind })}>
            <option value="chat">生成回答（chat）</option>
            <option value="embedding">把文字转向量（embedding）</option>
            <option value="rerank">结果重排（rerank）</option>
          </Select>
        </Field>
        <Field label="上下文长度（Token）" hint={contextWarning ? "生成模型低于 1M，后端会拒绝保存。" : "一次能读入多长的内容。生成模型要求至少 100 万。"} error={contextWarning ? "生成模型上下文必须 ≥ 1,000,000" : null}>
          <Input type="number" min={1} value={form.contextWindowTokens} onChange={(event) => setForm({ ...form, contextWindowTokens: event.target.value })} required />
        </Field>
        <Field label="单次最长输出（Token）">
          <Input type="number" min={1} value={form.maxOutputTokens} onChange={(event) => setForm({ ...form, maxOutputTokens: event.target.value })} required />
        </Field>
        <Field label="数据区域" hint="例如 cn / us，用于按合规要求筛选模型。">
          <Input value={form.dataRegion} onChange={(event) => setForm({ ...form, dataRegion: event.target.value })} required />
        </Field>
        <Field label="输入价格 / 百万 Token" hint="填 0 表示不计费；仅用于预算与展示。">
          <Input type="number" min={0} step="0.01" value={form.inputPrice} onChange={(event) => setForm({ ...form, inputPrice: event.target.value })} required />
        </Field>
        <Field label="输出价格 / 百万 Token">
          <Input type="number" min={0} step="0.01" value={form.outputPrice} onChange={(event) => setForm({ ...form, outputPrice: event.target.value })} required />
        </Field>
        <Field label="核验来源" hint={verificationWarning ? "生成模型必须填写来源与时间，才能被发布使用。" : "官方文档或内部评测链接。"} error={verificationWarning ? "需要核验来源与核验时间" : null}>
          <Input value={form.verificationSource} onChange={(event) => setForm({ ...form, verificationSource: event.target.value })} placeholder="https://docs.example.com/models" />
        </Field>
        <Field label="核验时间">
          <Input type="datetime-local" value={form.verifiedAt} onChange={(event) => setForm({ ...form, verifiedAt: event.target.value })} />
        </Field>
        <Field label="状态">
          <Select value={form.status} onChange={(event) => setForm({ ...form, status: event.target.value as ModelFormState["status"] })}>
            <option value="enabled">启用</option>
            <option value="disabled">停用</option>
          </Select>
        </Field>
        <fieldset className="flex flex-wrap items-center gap-4 text-sm text-ink-soft">
          <legend className="mb-1 block w-full text-sm font-medium text-ink">能力</legend>
          {([
            ["streaming", "流式"],
            ["tools", "工具调用"],
            ["structuredOutput", "结构化 JSON"],
          ] as const).map(([key, label]) => (
            <label key={key} className="flex items-center gap-2">
              <input type="checkbox" checked={form[key]} onChange={(event) => setForm({ ...form, [key]: event.target.checked })} />
              {label}
            </label>
          ))}
        </fieldset>
        {error ? (
          <p role="alert" className="rounded-lg border border-rust bg-rust-soft px-3 py-2 text-xs text-rust md:col-span-2">
            {error}
          </p>
        ) : null}
        <div className="flex justify-end gap-2 md:col-span-2">
          <Button type="button" variant="secondary" onClick={onCancel}>取消</Button>
          <Button type="submit" loading={busy} disabled={contextWarning || verificationWarning}>
            {initial ? "保存修改" : "登记模型"}
          </Button>
        </div>
      </form>
    </Card>
  );
}

// healthTone 将熔断状态映射为徽章色调。
function healthTone(status: ModelHealth["status"] | undefined): "success" | "warning" | "danger" | "info" | "neutral" {
  switch (status) {
    case "healthy":
      return "success";
    case "degraded":
      return "warning";
    case "half_open":
      return "info";
    case "open":
      return "danger";
    default:
      return "neutral";
  }
}

// healthLabel 输出熔断状态文案。
function healthLabel(status: ModelHealth["status"] | undefined): string {
  switch (status) {
    case "healthy":
      return "健康";
    case "degraded":
      return "失败累积";
    case "half_open":
      return "半开探测";
    case "open":
      return "熔断打开";
    case "unknown":
      return "状态未知";
    default:
      return "未读取";
  }
}

export default function ModelsPage() {
  const providers = useRemote(() => requestJSON<Page<Provider>>("/api/v1/providers"), []);
  const [providerFilter, setProviderFilter] = useState("");
  const models = useRemote(
    () => requestJSON<Page<Model>>(`/api/v1/models${providerFilter ? `?provider_id=${providerFilter}` : ""}`),
    [providerFilter],
  );
  const health = useRemote(() => requestJSON<{ items: ModelHealth[] }>("/api/v1/model-routing/health"), []);
  const [editing, setEditing] = useState<Model | null | "new">(null);
  const [deleting, setDeleting] = useState<Model | null>(null);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [batchOpen, setBatchOpen] = useState(false);
  const [batchError, setBatchError] = useState<string | null>(null);

  const remove = async () => {
    if (!deleting) {
      return;
    }
    setBusy(true);
    setDeleteError(null);
    try {
      await mutate<void>(`/api/v1/models/${deleting.id}`, "DELETE");
      setDeleting(null);
      models.reload();
    } catch (deleteFailure) {
      setDeleteError(describeError(deleteFailure));
    } finally {
      setBusy(false);
    }
  };

  const items = models.data?.items ?? [];
  const allSelected = items.length > 0 && items.every((model) => selected.has(model.id));
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
    setSelected(allSelected ? new Set() : new Set(items.map((model) => model.id)));
  };

  // 批量删除逐条调用现有接口：被规则引用的模型会失败，其余模型继续删除。
  const removeSelected = async () => {
    const targets = items.filter((model) => selected.has(model.id));
    if (targets.length === 0) {
      return;
    }
    setBusy(true);
    setBatchError(null);
    const failures: string[] = [];
    for (const model of targets) {
      try {
        await mutate<void>(`/api/v1/models/${model.id}`, "DELETE");
      } catch (failure) {
        failures.push(`${model.remote_model_name}：${describeError(failure)}`);
      }
    }
    setBusy(false);
    setBatchOpen(false);
    setSelected(new Set());
    models.reload();
    if (failures.length > 0) {
      setBatchError(`部分模型未能删除：${failures.join("；")}`);
    }
  };

  const providerNames = new Map(providers.data?.items.map((provider) => [provider.id, provider.name]) ?? []);
  const healthByModel = new Map(health.data?.items.map((item) => [item.model_id, item]) ?? []);

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        eyebrow="平台设置 · 第二步"
        title="可用模型"
        description="这里是“具体能调用哪个模型”。登记并核验后，才能在选模规则里安排主选与替补。"
        actions={<Button onClick={() => setEditing("new")} disabled={!providers.data?.items.length}>登记模型</Button>}
      />
      {providers.error ? <ErrorBanner message={providers.error} onRetry={providers.reload} /> : null}
      {models.error ? <ErrorBanner message={models.error} onRetry={models.reload} /> : null}
      {health.error ? <ErrorBanner message={`熔断状态不可用：${health.error}`} onRetry={health.reload} /> : null}
      {batchError ? <ErrorBanner message={batchError} /> : null}
      {editing ? (
        <ModelForm
          initial={editing === "new" ? null : editing}
          providers={providers.data?.items ?? []}
          onCancel={() => setEditing(null)}
          onSaved={() => { setEditing(null); models.reload(); }}
        />
      ) : null}
      <Card
        title="已登记模型"
        description="“当前可用性”来自运行时的熔断状态；未核验或上下文不足的生成模型不会被规则选中。"
        actions={
          <>
            {selected.size > 0 ? (
              <>
                <span className="text-xs text-muted">已选 {selected.size} 项</span>
                <Button size="sm" variant="danger" onClick={() => setBatchOpen(true)}>批量删除</Button>
              </>
            ) : null}
            <Select className="w-48" value={providerFilter} onChange={(event) => { setProviderFilter(event.target.value); setSelected(new Set()); }} aria-label="按供应商筛选">
              <option value="">全部供应商</option>
              {providers.data?.items.map((provider) => (
                <option key={provider.id} value={provider.id}>{provider.name}</option>
              ))}
            </Select>
          </>
        }
      >
        {models.loading ? (
          <LoadingState />
        ) : models.data && models.data.items.length > 0 ? (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[900px] text-left text-sm">
              <thead className="text-xs uppercase tracking-wide text-muted">
                <tr>
                  <th className="w-8 pb-3 pr-2 font-medium">
                    <input type="checkbox" checked={allSelected} onChange={toggleAll} aria-label="全选可用模型" />
                  </th>
                  <th className="pb-3 pr-4 font-medium">模型</th>
                  <th className="pb-3 pr-4 font-medium">来自服务</th>
                  <th className="pb-3 pr-4 font-medium">用途</th>
                  <th className="pb-3 pr-4 font-medium">上下文长度</th>
                  <th className="pb-3 pr-4 font-medium">核验状态</th>
                  <th className="pb-3 pr-4 font-medium">价格 / 百万</th>
                  <th className="pb-3 pr-4 font-medium">当前可用性</th>
                  <th className="pb-3 pr-4 font-medium">启用</th>
                  <th className="pb-3 font-medium">操作</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-line">
                {models.data.items.map((model) => (
                  <tr key={model.id}>
                    <td className="py-3 pr-2">
                      <input
                        type="checkbox"
                        checked={selected.has(model.id)}
                        onChange={() => toggleSelected(model.id)}
                        aria-label={`选择模型 ${model.remote_model_name}`}
                      />
                    </td>
                    <td className="py-3 pr-4">
                      <p className="font-medium text-ink">{model.remote_model_name}</p>
                      <p className="font-mono text-[11px] text-muted">{model.id.slice(0, 8)}</p>
                    </td>
                    <td className="py-3 pr-4 text-ink-soft">{providerNames.get(model.provider_id) ?? "—"}</td>
                    <td className="py-3 pr-4"><Badge tone="neutral">{model.kind}</Badge></td>
                    <td className="py-3 pr-4">
                      <p className={model.kind === "chat" && model.context_window_tokens < 1_000_000 ? "text-rust" : "text-ink"}>
                        {formatNumber(model.context_window_tokens)}
                      </p>
                      <p className="text-[11px] text-muted">输出上限 {formatNumber(model.max_output_tokens)}</p>
                    </td>
                    <td className="py-3 pr-4 text-xs text-muted">
                      {model.verified_at ? (
                        <>
                          <p>{formatDateTime(model.verified_at)}</p>
                          <p className="max-w-[180px] truncate" title={model.verification_source}>{model.verification_source}</p>
                        </>
                      ) : (
                        <Badge tone="warning">未核验</Badge>
                      )}
                    </td>
                    <td className="py-3 pr-4 text-xs text-ink-soft">
                      入 ${model.input_price_per_million} / 出 ${model.output_price_per_million}
                    </td>
                    <td className="py-3 pr-4">
                      {(() => {
                        const healthItem = healthByModel.get(model.id);
                        return (
                          <div className="flex flex-col gap-1">
                            <Badge tone={healthTone(healthItem?.status)}>{healthLabel(healthItem?.status)}</Badge>
                            {healthItem && healthItem.failures > 0 ? <span className="text-[11px] text-muted">失败 {healthItem.failures} 次</span> : null}
                          </div>
                        );
                      })()}
                    </td>
                    <td className="py-3 pr-4"><Badge tone={statusTone(model.status)}>{statusLabel(model.status)}</Badge></td>
                    <td className="py-3">
                      <div className="flex gap-2">
                        <Button size="sm" variant="secondary" onClick={() => setEditing(model)}>编辑</Button>
                        <Button size="sm" variant="danger" onClick={() => { setDeleting(model); setDeleteError(null); }}>删除</Button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <EmptyState title="还没有可用模型" description="先接入模型服务，再登记至少一个已核验的生成模型。" action={<Button size="sm" onClick={() => setEditing("new")} disabled={!providers.data?.items.length}>登记模型</Button>} />
        )}
      </Card>
      <ConfirmDialog
        open={deleting !== null}
        title={`删除模型 ${deleting?.remote_model_name ?? ""}？`}
        description="被发布策略引用的模型不能删除；请先调整策略版本。"
        confirmLabel="确认删除"
        busy={busy}
        onCancel={() => setDeleting(null)}
        onConfirm={() => void remove()}
      >
        {deleteError ? <p role="alert" className="text-xs text-rust">{deleteError}</p> : null}
      </ConfirmDialog>
      <ConfirmDialog
        open={batchOpen}
        title={`删除选中的 ${selected.size} 个模型？`}
        description="删除会逐条执行。被选模规则引用的模型会失败，并在完成后列出原因；其余模型仍会正常删除。"
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
