// 供应商管理：普通查询不返回密钥，编辑时可授权查看，删除需二次确认。
import { useCallback, useState } from "react";
import { FormDrawer } from "../../components/FormDrawer";
import { useToast } from "../../components/Toast";
import { Badge, Button, Card, ConfirmDialog, EmptyState, ErrorBanner, Field, Input, LoadingState, MenuItem, PageHeader, Pager, RowMenu, Select } from "../../components/ui";
import { mutate, requestJSON } from "../../lib/api";
import { describeError } from "../../lib/errors";
import { formatDateTime, statusLabel, statusTone } from "../../lib/format";
import { useClientPage, useRemote } from "../../lib/hooks";
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
  const initialForm: ProviderFormState =
    initial
      ? { name: initial.name, baseUrl: initial.base_url, apiKey: "", status: initial.status, dataRegion: initial.data_region }
      : emptyProvider;
  const [form, setForm] = useState(initialForm);
  const toast = useToast();
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const revealCredential = useCallback(
    () => requestJSON<{ configured: boolean; value?: string }>(`/api/v1/providers/${initial?.id}/reveal-credential`, {
      method: "POST",
      cache: "no-store",
      body: JSON.stringify({}),
    }),
    [initial?.id],
  );

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
      toast.show(initial ? "修改已保存" : "已接入服务", "success");
      onSaved();
    } catch (submitError) {
      setError(describeError(submitError));
    } finally {
      setBusy(false);
    }
  };

  return (
    <FormDrawer
      title={initial ? `编辑服务：${initial.name}` : "接入模型服务"} description="通常只需配置一次。密钥会加密保存，普通查询不返回明文，编辑时可授权查看。"
      submitLabel={initial ? "保存修改" : "接入服务"}
      busy={busy}
      dirty={JSON.stringify(form) !== JSON.stringify(initialForm)}
      error={error}
      onSubmit={submit}
      onCancel={onCancel}
    >
      <Field label="服务名称" hint="自己看得懂即可，例如“DeepSeek 官方”“公司网关”。">
        <Input value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} required maxLength={128} placeholder="例如：DeepSeek 官方" />
      </Field>
      <Field label="接口地址" hint="模型服务的 OpenAI 兼容地址，通常以 /v1 结尾。必须使用 HTTPS。">
        <Input type="url" value={form.baseUrl} onChange={(event) => setForm({ ...form, baseUrl: event.target.value })} required placeholder="https://api.example.com/v1" />
      </Field>
      <Field label="访问密钥" hint={initial ? "留空表示保留现有密钥；可点击眼睛授权查看，填写则替换并轮换。" : "从模型服务商控制台复制，普通查询不返回明文。"}>
        <Input type="password" autoComplete="off" value={form.apiKey} onChange={(event) => setForm({ ...form, apiKey: event.target.value })} onRevealSaved={initial ? revealCredential : undefined} required={!initial} placeholder="sk-..." />
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
    </FormDrawer>
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
  const paging = useClientPage(items, 10);
  const allSelected = paging.pageItems.length > 0 && paging.pageItems.every((provider) => selected.has(provider.id));
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
    const pageIDs = new Set(paging.pageItems.map((provider) => provider.id));
    setSelected((current) => {
      const next = new Set(current);
      if (paging.pageItems.every((provider) => next.has(provider.id))) {
        pageIDs.forEach((id) => next.delete(id));
      } else {
        pageIDs.forEach((id) => next.add(id));
      }
      return next;
    });
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
        actions={<Button aria-haspopup="dialog" onClick={() => setEditing("new")}>接入模型服务</Button>}
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
        {loading && !data ? (
          <LoadingState />
        ) : data && data.items.length > 0 ? (
          <>
          <div className="admin-table-container overflow-x-auto">
            <label className="admin-table-mobile-select min-h-10 items-center gap-2 border-b border-line text-sm text-muted">
              <input type="checkbox" checked={allSelected} onChange={toggleAll} />
              全选本页
            </label>
            <table role="table" className="admin-table w-full min-w-[720px] text-left text-sm">
              <thead className="text-xs uppercase tracking-wide text-muted">
                <tr>
                  <th className="w-8 pb-3 pr-2 font-medium">
                    <input className="admin-table-desktop-select" type="checkbox" checked={allSelected} onChange={toggleAll} aria-label="全选模型服务" />
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
              <tbody role="rowgroup" className="divide-y divide-line">
                {paging.pageItems.map((provider) => (
                  <tr role="row" key={provider.id}>
                    <td role="cell" data-row="select" className="py-3 pr-2">
                      <input
                        type="checkbox"
                        checked={selected.has(provider.id)}
                        onChange={() => toggleSelected(provider.id)}
                        aria-label={`选择模型服务 ${provider.name}`}
                      />
                    </td>
                    <td role="cell" data-row="main" className="py-3 pr-4 font-medium text-ink">{provider.name}</td>
                    <td role="cell" data-column="接口地址" className="max-w-[220px] truncate py-3 pr-4 font-mono text-xs text-muted">{provider.base_url}</td>
                    <td role="cell" data-column="数据区域" className="py-3 pr-4 text-ink-soft">{provider.data_region || "—"}</td>
                    <td role="cell" data-column="密钥版本" className="py-3 pr-4 text-ink-soft">v{provider.key_version}</td>
                    <td role="cell" data-column="状态" className="py-3 pr-4"><Badge tone={statusTone(provider.status)}>{statusLabel(provider.status)}</Badge></td>
                    <td role="cell" data-column="更新时间" className="py-3 pr-4 text-xs text-muted">{formatDateTime(provider.updated_at)}</td>
                    <td role="cell" data-row="actions" className="py-3">
                      <RowMenu name={provider.name}>
                        <MenuItem icon="edit" onClick={() => setEditing(provider)}>编辑</MenuItem>
                        <MenuItem icon="trash" danger onClick={() => { setDeleting(provider); setDeleteError(null); }}>删除</MenuItem>
                      </RowMenu>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          </>
        ) : (
          <EmptyState title="还没有模型服务" description="接入一个 OpenAI 兼容服务后，就能在下一步登记它提供的模型。" action={<Button size="sm" aria-haspopup="dialog" onClick={() => setEditing("new")}>接入模型服务</Button>} />
        )}
        <Pager page={paging.page} totalPages={paging.totalPages} total={items.length} busy={loading} onChange={paging.setPage} />
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
