// 供应商管理：密钥只写不读，删除需二次确认。
import { useState } from "react";
import { Badge, Button, Card, ConfirmDialog, EmptyState, ErrorBanner, Field, Input, LoadingState, PageHeader, Select } from "../../components/ui";
import { mutate, requestJSON } from "../../lib/api";
import { describeError } from "../../lib/errors";
import { formatDateTime, statusLabel, statusTone } from "../../lib/format";
import { useRemote } from "../../lib/hooks";
import type { Page, Provider } from "../../lib/types";

interface ProviderFormState {
  name: string;
  baseUrl: string;
  apiKey: string;
  status: "enabled" | "disabled";
  dataRegion: string;
}

const emptyProvider: ProviderFormState = { name: "", baseUrl: "", apiKey: "", status: "enabled", dataRegion: "cn" };

function ProviderForm({ initial, onSaved, onCancel }: { initial: Provider | null; onSaved: () => void; onCancel: () => void }) {
  const [form, setForm] = useState<ProviderFormState>(
    initial
      ? { name: initial.name, baseUrl: initial.base_url, apiKey: "", status: initial.status, dataRegion: initial.data_region }
      : emptyProvider,
  );
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const body = {
        name: form.name.trim(),
        base_url: form.baseUrl.trim(),
        api_key: form.apiKey.trim(),
        status: form.status,
        data_region: form.dataRegion.trim(),
      };
      if (initial) {
        await mutate<Provider>(`/api/v1/providers/${initial.id}`, "PUT", body, crypto.randomUUID());
      } else {
        await mutate<Provider>("/api/v1/providers", "POST", body, crypto.randomUUID());
      }
      onSaved();
    } catch (submitError) {
      setError(describeError(submitError));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card title={initial ? `编辑服务：${initial.name}` : "接入模型服务"} description="通常只需配置一次。密钥会加密保存，保存后不再显示明文。">
      <form onSubmit={submit} className="grid gap-4 md:grid-cols-2">
        <Field label="服务名称" hint="自己看得懂即可，例如“DeepSeek 官方”“公司网关”。">
          <Input value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} required maxLength={128} placeholder="例如：DeepSeek 官方" />
        </Field>
        <Field label="接口地址" hint="模型服务的 OpenAI 兼容地址，通常以 /v1 结尾。必须使用 HTTPS。">
          <Input type="url" value={form.baseUrl} onChange={(event) => setForm({ ...form, baseUrl: event.target.value })} required placeholder="https://api.example.com/v1" />
        </Field>
        <Field label="访问密钥" hint={initial ? "留空表示保留现有密钥；填写则替换并轮换。" : "从模型服务商控制台复制，只保存一次。"}>
          <Input type="password" autoComplete="off" value={form.apiKey} onChange={(event) => setForm({ ...form, apiKey: event.target.value })} required={!initial} placeholder="sk-..." />
        </Field>
        <Field label="数据区域" hint="标记数据存放地区，例如 cn / us，用于合规筛选。">
          <Input value={form.dataRegion} onChange={(event) => setForm({ ...form, dataRegion: event.target.value })} required maxLength={64} placeholder="例如：cn" />
        </Field>
        <Field label="状态">
          <Select value={form.status} onChange={(event) => setForm({ ...form, status: event.target.value as ProviderFormState["status"] })}>
            <option value="enabled">启用</option>
            <option value="disabled">停用</option>
          </Select>
        </Field>
        {error ? (
          <p role="alert" className="rounded-lg border border-rust bg-rust-soft px-3 py-2 text-xs text-rust md:col-span-2">
            {error}
          </p>
        ) : null}
        <div className="flex justify-end gap-2 md:col-span-2">
          <Button type="button" variant="secondary" onClick={onCancel}>
            取消
          </Button>
          <Button type="submit" loading={busy}>
            {initial ? "保存修改" : "接入服务"}
          </Button>
        </div>
      </form>
    </Card>
  );
}

export default function ProvidersPage() {
  const { data, loading, error, reload } = useRemote(() => requestJSON<Page<Provider>>("/api/v1/providers"), []);
  const [editing, setEditing] = useState<Provider | null | "new">(null);
  const [deleting, setDeleting] = useState<Provider | null>(null);
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
      await mutate<void>(`/api/v1/providers/${deleting.id}`, "DELETE");
      setDeleting(null);
      reload();
    } catch (deleteFailure) {
      setDeleteError(describeError(deleteFailure));
    } finally {
      setBusy(false);
    }
  };

  const items = data?.items ?? [];
  const allSelected = items.length > 0 && items.every((provider) => selected.has(provider.id));
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
    setSelected(allSelected ? new Set() : new Set(items.map((provider) => provider.id)));
  };

  // 批量删除复用单条接口，逐条执行并汇总失败项，避免一条失败中断其余删除。
  const removeSelected = async () => {
    const targets = items.filter((provider) => selected.has(provider.id));
    if (targets.length === 0) {
      return;
    }
    setBusy(true);
    setBatchError(null);
    const failures: string[] = [];
    for (const provider of targets) {
      try {
        await mutate<void>(`/api/v1/providers/${provider.id}`, "DELETE");
      } catch (failure) {
        failures.push(`${provider.name}：${describeError(failure)}`);
      }
    }
    setBusy(false);
    setBatchOpen(false);
    setSelected(new Set());
    reload();
    if (failures.length > 0) {
      setBatchError(`部分服务未能删除：${failures.join("；")}`);
    }
  };

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        eyebrow="平台设置 · 第一步"
        title="模型服务"
        description="这里是“去哪儿调用模型”。填好服务地址和密钥后，下一步到“可用模型”登记具体模型。"
        actions={<Button onClick={() => setEditing("new")}>接入模型服务</Button>}
      />
      {error ? <ErrorBanner message={error} onRetry={reload} /> : null}
      {batchError ? <ErrorBanner message={batchError} /> : null}
      {editing ? <ProviderForm initial={editing === "new" ? null : editing} onCancel={() => setEditing(null)} onSaved={() => { setEditing(null); reload(); }} /> : null}
      <Card
        title="已接入的服务"
        description={`共 ${data?.items.length ?? 0} 个服务。停用后，新对话不会再选择它下面的模型。`}
        actions={
          selected.size > 0 ? (
            <>
              <span className="text-xs text-muted">已选 {selected.size} 项</span>
              <Button size="sm" variant="danger" onClick={() => setBatchOpen(true)}>批量删除</Button>
            </>
          ) : null
        }
      >
        {loading ? (
          <LoadingState />
        ) : data && data.items.length > 0 ? (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[720px] text-left text-sm">
              <thead className="text-xs uppercase tracking-wide text-muted">
                <tr>
                  <th className="w-8 pb-3 pr-2 font-medium">
                    <input type="checkbox" checked={allSelected} onChange={toggleAll} aria-label="全选模型服务" />
                  </th>
                  <th className="pb-3 pr-4 font-medium">名称</th>
                  <th className="pb-3 pr-4 font-medium">基础地址</th>
                  <th className="pb-3 pr-4 font-medium">区域</th>
                  <th className="pb-3 pr-4 font-medium">密钥版本</th>
                  <th className="pb-3 pr-4 font-medium">状态</th>
                  <th className="pb-3 pr-4 font-medium">更新时间</th>
                  <th className="pb-3 font-medium">操作</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-line">
                {data.items.map((provider) => (
                  <tr key={provider.id}>
                    <td className="py-3 pr-2">
                      <input
                        type="checkbox"
                        checked={selected.has(provider.id)}
                        onChange={() => toggleSelected(provider.id)}
                        aria-label={`选择模型服务 ${provider.name}`}
                      />
                    </td>
                    <td className="py-3 pr-4 font-medium text-ink">{provider.name}</td>
                    <td className="max-w-[220px] truncate py-3 pr-4 font-mono text-xs text-muted">{provider.base_url}</td>
                    <td className="py-3 pr-4 text-ink-soft">{provider.data_region || "—"}</td>
                    <td className="py-3 pr-4 text-ink-soft">v{provider.key_version}</td>
                    <td className="py-3 pr-4"><Badge tone={statusTone(provider.status)}>{statusLabel(provider.status)}</Badge></td>
                    <td className="py-3 pr-4 text-xs text-muted">{formatDateTime(provider.updated_at)}</td>
                    <td className="py-3">
                      <div className="flex gap-2">
                        <Button size="sm" variant="secondary" onClick={() => setEditing(provider)}>编辑</Button>
                        <Button size="sm" variant="danger" onClick={() => { setDeleting(provider); setDeleteError(null); }}>删除</Button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <EmptyState title="还没有模型服务" description="接入一个 OpenAI 兼容服务后，就能在下一步登记它提供的模型。" action={<Button size="sm" onClick={() => setEditing("new")}>接入模型服务</Button>} />
        )}
      </Card>
      <ConfirmDialog
        open={deleting !== null}
        title={`删除供应商 ${deleting?.name ?? ""}？`}
        description="该操作不可撤销；引用此供应商的模型需要先清理，否则后端会拒绝删除。"
        confirmLabel="确认删除"
        busy={busy}
        onCancel={() => setDeleting(null)}
        onConfirm={() => void remove()}
      >
        {deleteError ? <p role="alert" className="text-xs text-rust">{deleteError}</p> : null}
      </ConfirmDialog>
      <ConfirmDialog
        open={batchOpen}
        title={`删除选中的 ${selected.size} 个模型服务？`}
        description="删除会逐条执行。被模型引用的服务会失败，并在完成后列出原因；其余服务仍会正常删除。"
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
