// 模型目录：展示上下文窗口核验、能力与价格，生成模型强制 1M+ 上下文。
import { useRef, useState } from "react";
import { FormDrawer } from "../../components/FormDrawer";
import { useToast } from "../../components/Toast";
import { Badge, Button, Card, ConfirmDialog, EmptyState, ErrorBanner, Field, Input, LoadingState, MenuItem, PageHeader, Pager, RowMenu, Select } from "../../components/ui";
import { mutate, requestJSON } from "../../lib/api";
import { describeError } from "../../lib/errors";
import { formatDateTime, formatNumber, statusLabel, statusTone } from "../../lib/format";
import { useClientPage, useRemote } from "../../lib/hooks";
import type { Model, ModelHealth, ModelKind, Page, Provider, RemoteModel, VectorStatus } from "../../lib/types";

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

// datetime-local 使用浏览器时区，提交时再转换为 UTC。
function toLocalInput(date: Date): string {
  if (!Number.isFinite(date.getTime())) return "";
  const local = new Date(date.getTime() - date.getTimezoneOffset() * 60_000);
  return local.toISOString().slice(0, 16);
}

function ModelForm({ initial, providers, onSaved, onCancel }: { initial: Model | null; providers: Provider[]; onSaved: () => void; onCancel: () => void }) {
  const [initialForm] = useState<ModelFormState>(() => {
    const now = toLocalInput(new Date());
    if (!initial) return { ...emptyModel, verifiedAt: now };
    const provider = providers.find((item) => item.id === initial.provider_id);
    const enabled = initial.status === "enabled";
    return {
      providerId: initial.provider_id,
      remoteModelName: initial.remote_model_name,
      kind: initial.kind,
      contextWindowTokens: String(initial.context_window_tokens),
      maxOutputTokens: String(initial.max_output_tokens),
      inputPrice: String(initial.input_price_per_million),
      outputPrice: String(initial.output_price_per_million),
      dataRegion: initial.data_region,
      verificationSource: initial.verification_source?.trim() ? initial.verification_source : enabled ? provider?.base_url ?? "" : "",
      verifiedAt: (initial.verified_at && toLocalInput(new Date(initial.verified_at))) || (enabled ? now : ""),
      status: initial.status,
      streaming: Boolean(initial.capabilities?.streaming),
      tools: Boolean(initial.capabilities?.tools),
      structuredOutput: Boolean(initial.capabilities?.structured_output),
    };
  });
  const [form, setForm] = useState(initialForm);
  const [sourceFollowsProvider, setSourceFollowsProvider] = useState(
    !initial || (initial.status === "enabled" && !initial.verification_source?.trim()),
  );
  const toast = useToast();
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [remoteModels, setRemoteModels] = useState<RemoteModel[]>([]);
  const [remoteBusy, setRemoteBusy] = useState(false);
  const [remoteError, setRemoteError] = useState<string | null>(null);
  const remoteRequest = useRef(0);
  const contextTokens = Number(form.contextWindowTokens || 0);
  const contextWarning = form.kind === "chat" && contextTokens < 1_000_000;
  const selectedProvider = providers.find((provider) => provider.id === form.providerId);
  const verificationSource = sourceFollowsProvider ? selectedProvider?.base_url ?? "" : form.verificationSource;
  const requiresVerification = form.status === "enabled";

  // pullRemoteModels 从供应商站点读取可用模型名称，避免手工抄写出错。
  const pullRemoteModels = async () => {
    if (!form.providerId) {
      setRemoteError("请先选择模型服务。");
      return;
    }
    const request = ++remoteRequest.current;
    setRemoteBusy(true);
    setRemoteError(null);
    try {
      const response = await requestJSON<{ items: RemoteModel[] }>(`/api/v1/providers/${form.providerId}/models`);
      if (request !== remoteRequest.current) return;
      setRemoteModels(response.items);
      if (response.items.length === 0) {
        setRemoteError("站点没有返回可用模型。");
      } else {
        setForm((current) => current.remoteModelName ? current : { ...current, remoteModelName: response.items[0].id });
      }
    } catch (failure) {
      if (request === remoteRequest.current) setRemoteError(describeError(failure));
    } finally {
      if (request === remoteRequest.current) setRemoteBusy(false);
    }
  };

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
        verification_source: verificationSource.trim(),
        verified_at: form.verifiedAt
          ? initial?.verified_at && form.verifiedAt === toLocalInput(new Date(initial.verified_at))
            ? initial.verified_at
            : new Date(form.verifiedAt).toISOString()
          : null,
        status: form.status,
      };
      if (initial) {
        await mutate<Model>(`/api/v1/models/${initial.id}`, "PUT", body, crypto.randomUUID());
      } else {
        await mutate<Model>("/api/v1/models", "POST", body, crypto.randomUUID());
      }
      toast.show(initial ? "修改已保存" : "已登记模型", "success");
      onSaved();
    } catch (submitError) {
      setError(describeError(submitError));
    } finally {
      setBusy(false);
    }
  };

  return (
    <FormDrawer
      title={initial ? `编辑模型：${initial.remote_model_name}` : "登记模型"} description="登记后，模型才能被选模规则引用。生成模型需要 1,000,000 以上上下文，并记录核验来源。"
      submitLabel={initial ? "保存修改" : "登记模型"}
      busy={busy}
      dirty={JSON.stringify({ ...form, verificationSource }) !== JSON.stringify(initialForm)}
      error={error}
      onSubmit={submit}
      onCancel={onCancel}
    >
      <Field label="来自哪个服务" hint="先接入模型服务，这里才能选择。">
        <Select value={form.providerId} onChange={(event) => {
          remoteRequest.current += 1;
          setForm({ ...form, providerId: event.target.value });
          setRemoteModels([]);
          setRemoteError(null);
          setRemoteBusy(false);
        }} required>
          <option value="">选择模型服务</option>
          {providers.map((provider) => (
            <option key={provider.id} value={provider.id}>{provider.name}</option>
          ))}
        </Select>
      </Field>
      <div className="min-w-0 sm:col-span-2">
        <Field label="模型名称" hint="填写服务商提供的模型 ID。">
          <Input className="min-w-0 flex-1" value={form.remoteModelName} onChange={(event) => setForm({ ...form, remoteModelName: event.target.value })} required placeholder="例如：deepseek-chat" />
        </Field>
        <Button type="button" size="sm" variant="secondary" className="mt-2" loading={remoteBusy} disabled={!form.providerId} onClick={() => void pullRemoteModels()}>
          从站点拉取模型
        </Button>
        {remoteModels.length > 0 ? (
          <Select
            className="mt-2"
            value=""
            onChange={(event) => {
              if (event.target.value) {
                setForm({ ...form, remoteModelName: event.target.value });
              }
            }}
            aria-label="选择拉取到的模型"
          >
            <option value="">选择拉取到的模型（{remoteModels.length} 个）</option>
            {remoteModels.map((model) => (
              <option key={model.id} value={model.id}>{model.id}{model.owned_by ? ` · ${model.owned_by}` : ""}</option>
            ))}
          </Select>
        ) : null}
        {remoteError ? <p role="alert" className="mt-1 text-xs text-rust">{remoteError}</p> : null}
      </div>
      <Field label="用它来做什么" hint="大多数助手只用“生成”；向量与重排用于记忆检索。">
        <Select value={form.kind} onChange={(event) => setForm({ ...form, kind: event.target.value as ModelKind })}>
          <option value="chat">生成回答（chat）</option>
          <option value="embedding">把文字转向量（embedding）</option>
          <option value="rerank">结果重排（rerank）</option>
        </Select>
      </Field>
      <Field label="上下文长度（Token）" hint={contextWarning ? "生成模型低于 1M，后端会拒绝保存。" : "一次能读入多长的内容。生成模型要求至少 100 万。"} error={contextWarning ? "生成模型上下文必须 ≥ 1,000,000" : null}>
        <Input type="number" min={form.kind === "chat" ? 1_000_000 : 1} value={form.contextWindowTokens} onChange={(event) => setForm({ ...form, contextWindowTokens: event.target.value })} required />
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
      <div className="min-w-0">
        <Field label="核验来源" hint="可使用模型服务地址，也可填写实际核验所用的文档或评测记录。">
          <Input required={requiresVerification} value={verificationSource} onChange={(event) => {
            setSourceFollowsProvider(false);
            setForm({ ...form, verificationSource: event.target.value });
          }} placeholder={sourceFollowsProvider ? "选择模型服务后自动填入" : "填写核验来源"} />
        </Field>
        {!sourceFollowsProvider && selectedProvider?.base_url ? (
          <Button size="sm" variant="ghost" onClick={() => {
            setSourceFollowsProvider(true);
            setForm({ ...form, verificationSource: selectedProvider.base_url });
          }}>使用服务地址</Button>
        ) : null}
      </div>
      <div className="min-w-0">
        <Field label="核验时间" hint="按本地时间填写，以实际核验时间为准。">
          <Input type="datetime-local" required={requiresVerification} value={form.verifiedAt} onChange={(event) => setForm({ ...form, verifiedAt: event.target.value })} />
        </Field>
        <Button size="sm" variant="ghost" onClick={() => setForm({ ...form, verifiedAt: toLocalInput(new Date()) })}>使用当前时间</Button>
      </div>
      <Field label="状态">
        <Select value={form.status} onChange={(event) => {
          const status = event.target.value as ModelFormState["status"];
          if (status === "enabled" && !verificationSource.trim()) setSourceFollowsProvider(true);
          setForm({ ...form, status, verifiedAt: form.verifiedAt || (status === "enabled" ? toLocalInput(new Date()) : "") });
        }}>
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
    </FormDrawer>
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
  const vectorStatus = useRemote(() => requestJSON<VectorStatus>("/api/v1/memory/vector-status"), []);
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
  const paging = useClientPage(items, 10);
  const allSelected = paging.pageItems.length > 0 && paging.pageItems.every((model) => selected.has(model.id));
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
    const pageIDs = new Set(paging.pageItems.map((model) => model.id));
    setSelected((current) => {
      const next = new Set(current);
      if (paging.pageItems.every((model) => next.has(model.id))) {
        pageIDs.forEach((id) => next.delete(id));
      } else {
        pageIDs.forEach((id) => next.add(id));
      }
      return next;
    });
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
  const vector = vectorStatus.data;

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        eyebrow="平台设置 · 第二步"
        title="可用模型"
        description="这里是“具体能调用哪个模型”。登记并核验后，才能在选模规则里安排主选与替补。"
        actions={<Button aria-haspopup="dialog" onClick={() => setEditing("new")} disabled={!providers.data?.items.length}>登记模型</Button>}
      />
      {providers.error ? <ErrorBanner message={providers.error} onRetry={providers.reload} /> : null}
      {models.error ? <ErrorBanner message={models.error} onRetry={models.reload} /> : null}
      {health.error ? <ErrorBanner message={`熔断状态不可用：${health.error}`} onRetry={health.reload} /> : null}
      {vectorStatus.error ? <ErrorBanner message={`向量模型状态不可用：${vectorStatus.error}`} onRetry={vectorStatus.reload} /> : null}
      {batchError ? <ErrorBanner message={batchError} /> : null}
      {editing ? (
        <ModelForm
          initial={editing === "new" ? null : editing}
          providers={providers.data?.items ?? []}
          onCancel={() => setEditing(null)}
          onSaved={() => { setEditing(null); models.reload(); vectorStatus.reload(); }}
        />
      ) : null}
      <Card
        title="向量模型状态"
        description="长期记忆检索依赖已启用的 embedding 模型。模型、启用状态与索引都就绪才算生效。"
        actions={
          <>
            {vector ? <Badge tone={vector.healthy ? "success" : "warning"}>{vector.healthy ? "已生效" : "未生效"}</Badge> : null}
            <Button size="sm" variant="secondary" loading={vectorStatus.loading} onClick={vectorStatus.reload}>重新检查</Button>
          </>
        }
      >
        {vectorStatus.loading ? (
          <p className="text-xs text-muted">正在检查向量模型配置…</p>
        ) : vector ? (
          <div className="flex flex-col gap-3">
            <dl className="grid gap-3 text-sm sm:grid-cols-2 lg:grid-cols-4">
              <div>
                <dt className="text-[11px] uppercase tracking-wide text-muted">模型服务</dt>
                <dd className="mt-1 text-ink">{vector.provider_name ?? "—"}</dd>
              </div>
              <div>
                <dt className="text-[11px] uppercase tracking-wide text-muted">向量模型</dt>
                <dd className={vector.model_configured ? "mt-1 text-ink" : "mt-1 text-rust"}>{vector.model ?? "未配置"}</dd>
              </div>
              <div>
                <dt className="text-[11px] uppercase tracking-wide text-muted">向量维度</dt>
                <dd className="mt-1 text-ink">{vector.dimension ? `${vector.dimension} 维` : "—"}</dd>
              </div>
              <div>
                <dt className="text-[11px] uppercase tracking-wide text-muted">索引 / 已写入</dt>
                <dd className="mt-1 text-ink">
                  {vector.index_version ?? "尚未创建"}
                  {vector.index_status ? ` · ${vector.index_status}` : ""}
                  {` · ${formatNumber(vector.vector_count)} 条`}
                </dd>
              </div>
            </dl>
            {vector.warning ? (
              <div role="status" className="flex flex-wrap items-center justify-between gap-3 rounded-lg border-l-4 border-ochre bg-ochre-soft/60 px-4 py-3 text-xs leading-5 text-ochre">
                <span>{vector.warning}</span>
                {!vector.model_configured ? (
                  <Button size="sm" variant="secondary" aria-haspopup="dialog" onClick={() => setEditing("new")} disabled={!providers.data?.items.length}>
                    登记 embedding 模型
                  </Button>
                ) : null}
              </div>
            ) : (
              <p className="text-xs leading-5 text-jade">模型与索引均已就绪，新的对话内容会自动写入向量记忆。</p>
            )}
          </div>
        ) : null}
      </Card>
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
            <div className="w-44 shrink-0">
              <Select className="w-full" value={providerFilter} onChange={(event) => { setProviderFilter(event.target.value); setSelected(new Set()); paging.reset(); }} aria-label="按供应商筛选">
                <option value="">全部供应商</option>
                {providers.data?.items.map((provider) => (
                  <option key={provider.id} value={provider.id}>{provider.name}</option>
                ))}
              </Select>
            </div>
          </>
        }
      >
        {models.loading && !models.data ? (
          <LoadingState />
        ) : models.data && models.data.items.length > 0 ? (
          <>
          <div className="admin-table-container overflow-x-auto">
            <label className="admin-table-mobile-select min-h-10 items-center gap-2 border-b border-line text-sm text-muted">
              <input type="checkbox" checked={allSelected} onChange={toggleAll} />
              全选本页
            </label>
            <table role="table" className="admin-table w-full min-w-[900px] text-left text-sm">
              <thead className="text-xs uppercase tracking-wide text-muted">
                <tr>
                  <th className="w-8 pb-3 pr-2 font-medium">
                    <input className="admin-table-desktop-select" type="checkbox" checked={allSelected} onChange={toggleAll} aria-label="全选可用模型" />
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
              <tbody role="rowgroup" className="divide-y divide-line">
                {paging.pageItems.map((model) => (
                  <tr role="row" key={model.id}>
                    <td role="cell" data-row="select" className="py-3 pr-2">
                      <input
                        type="checkbox"
                        checked={selected.has(model.id)}
                        onChange={() => toggleSelected(model.id)}
                        aria-label={`选择模型 ${model.remote_model_name}`}
                      />
                    </td>
                    <td role="cell" data-row="main" className="py-3 pr-4">
                      <p className="font-medium text-ink">{model.remote_model_name}</p>
                      <p className="font-mono text-[11px] text-muted">{model.id.slice(0, 8)}</p>
                    </td>
                    <td role="cell" data-column="模型服务" className="py-3 pr-4 text-ink-soft">{providerNames.get(model.provider_id) ?? "—"}</td>
                    <td role="cell" data-column="用途" className="py-3 pr-4"><Badge tone="neutral">{model.kind}</Badge></td>
                    <td role="cell" data-column="上下文" className="py-3 pr-4">
                      <div>
                      <p className={model.kind === "chat" && model.context_window_tokens < 1_000_000 ? "text-rust" : "text-ink"}>
                        {formatNumber(model.context_window_tokens)}
                      </p>
                      <p className="text-[11px] text-muted">输出上限 {formatNumber(model.max_output_tokens)}</p>
                      </div>
                    </td>
                    <td role="cell" data-column="核验状态" className="py-3 pr-4 text-xs text-muted">
                      {model.verified_at ? (
                        <>
                          <p>{formatDateTime(model.verified_at)}</p>
                          <p className="max-w-[180px] truncate" title={model.verification_source}>{model.verification_source}</p>
                        </>
                      ) : (
                        <Badge tone="warning">未核验</Badge>
                      )}
                    </td>
                    <td role="cell" data-column="价格 / 百万" className="py-3 pr-4 text-xs text-ink-soft">
                      入 ${model.input_price_per_million} / 出 ${model.output_price_per_million}
                    </td>
                    <td role="cell" data-column="可用性" className="py-3 pr-4">
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
                    <td role="cell" data-column="状态" className="py-3 pr-4"><Badge tone={statusTone(model.status)}>{statusLabel(model.status)}</Badge></td>
                    <td role="cell" data-row="actions" className="py-3">
                      <RowMenu name={model.remote_model_name}>
                        <MenuItem icon="edit" onClick={() => setEditing(model)}>编辑</MenuItem>
                        <MenuItem icon="trash" danger onClick={() => { setDeleting(model); setDeleteError(null); }}>删除</MenuItem>
                      </RowMenu>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          </>
        ) : (
          <EmptyState title="还没有可用模型" description="先接入模型服务，再登记至少一个已核验的生成模型。" action={<Button size="sm" aria-haspopup="dialog" onClick={() => setEditing("new")} disabled={!providers.data?.items.length}>登记模型</Button>} />
        )}
        <Pager page={paging.page} totalPages={paging.totalPages} total={items.length} busy={models.loading} onChange={paging.setPage} />
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
