// MCP 接入管理：仅允许 HTTPS 公网端点，凭证只写不读，风险等级决定审批策略。
import { useState } from "react";
import { Badge, Button, Card, ConfirmDialog, EmptyState, ErrorBanner, Field, Input, LoadingState, PageHeader, Pager, Select, Textarea } from "../../components/ui";
import { mutate, requestJSON } from "../../lib/api";
import { describeError } from "../../lib/errors";
import { formatDateTime, statusLabel, statusTone } from "../../lib/format";
import { useClientPage, useRemote } from "../../lib/hooks";
import type { MCP, MCPStatus, Page } from "../../lib/types";

interface MCPFormState {
  name: string;
  endpoint: string;
  authType: "none" | "bearer" | "oauth" | "header";
  credentials: string;
  riskLevel: "low" | "medium" | "high";
  status: MCPStatus;
}

const emptyMCP: MCPFormState = { name: "", endpoint: "", authType: "none", credentials: "", riskLevel: "low", status: "enabled" };

function MCPForm({ initial, onSaved, onCancel }: { initial: MCP | null; onSaved: () => void; onCancel: () => void }) {
  const [form, setForm] = useState<MCPFormState>(
    initial
      ? { name: initial.name, endpoint: initial.endpoint, authType: (initial.auth_type as MCPFormState["authType"]) ?? "none", credentials: "", riskLevel: (initial.risk_level as MCPFormState["riskLevel"]) ?? "low", status: initial.status }
      : emptyMCP,
  );
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const needsCredential = form.authType !== "none";

  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    setError(null);
    // 自定义请求头必须保存为 JSON 对象，避免把整段文本误当成单个请求头值。
    if (form.authType === "header" && form.credentials.trim()) {
      try {
        const parsed = JSON.parse(form.credentials);
        if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
          throw new Error("not an object");
        }
      } catch {
        setError('请求头必须是 JSON 对象，例如 {"X-Api-Key":"your-key","X-Tenant":"team-a"}');
        return;
      }
    }
    setBusy(true);
    try {
      const body = {
        name: form.name.trim(),
        endpoint: form.endpoint.trim(),
        auth_type: form.authType,
        credentials: form.credentials,
        risk_level: form.riskLevel,
        status: form.status,
      };
      if (initial) {
        await mutate<MCP>(`/api/v1/mcps/${initial.id}`, "PUT", body, crypto.randomUUID());
      } else {
        await mutate<MCP>("/api/v1/mcps", "POST", body, crypto.randomUUID());
      }
      onSaved();
    } catch (submitError) {
      setError(describeError(submitError));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card title={initial ? `编辑外部工具：${initial.name}` : "接入外部工具"} description="外部工具让助手能查询或操作其他系统。地址必须是 HTTPS 公网地址，内网地址会被拒绝。">
      <form onSubmit={submit} className="grid gap-4 md:grid-cols-2">
        <Field label="工具名称" hint="例如“知识库检索”“工单查询”。">
          <Input value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} required maxLength={128} />
        </Field>
        <Field label="鉴权方式" hint="按工具服务要求选择；密钥会加密保存。">
          <Select value={form.authType} onChange={(event) => setForm({ ...form, authType: event.target.value as MCPFormState["authType"] })}>
            <option value="none">无需鉴权</option>
            <option value="bearer">Bearer Token</option>
            <option value="header">自定义请求头</option>
            <option value="oauth">OAuth</option>
          </Select>
        </Field>
        <div className="md:col-span-2">
          <Field label="工具地址" hint="工具服务提供的 MCP 端点，例如 https://mcp.example.com/sse">
            <Input type="url" value={form.endpoint} onChange={(event) => setForm({ ...form, endpoint: event.target.value })} required placeholder="https://mcp.example.com/sse" />
          </Field>
        </div>
        {form.authType === "header" ? (
          <div className="md:col-span-2">
            <Field
              label="请求头 JSON"
              hint={initial ? "留空保留现有请求头；填写则整体替换，必须是 JSON 对象。" : '按 JSON 对象填写，例如 {"X-Api-Key":"your-key","X-Tenant":"team-a"}。'}
            >
              <Textarea
                className="min-h-28 font-mono text-xs"
                value={form.credentials}
                onChange={(event) => setForm({ ...form, credentials: event.target.value })}
                required={!initial}
                spellCheck={false}
                placeholder={'{\n  "X-Api-Key": "your-key"\n}'}
              />
            </Field>
          </div>
        ) : form.authType === "none" ? null : (
          <Field label={form.authType === "bearer" ? "Bearer Token" : "OAuth 凭证"} hint={initial ? "留空保留现有凭证；填写则替换。" : "保存后不再显示明文。"}>
            <Input type="password" autoComplete="off" value={form.credentials} onChange={(event) => setForm({ ...form, credentials: event.target.value })} required={!initial} />
          </Field>
        )}
        <Field label="风险等级" hint="中高风险工具在每次执行前都会请求你审批。">
          <Select value={form.riskLevel} onChange={(event) => setForm({ ...form, riskLevel: event.target.value as MCPFormState["riskLevel"] })}>
            <option value="low">低：只读查询</option>
            <option value="medium">中：可能修改数据</option>
            <option value="high">高：有副作用或涉及外部系统</option>
          </Select>
        </Field>
        <Field label="状态">
          <Select value={form.status} onChange={(event) => setForm({ ...form, status: event.target.value as MCPStatus })}>
            <option value="enabled">启用</option>
            <option value="disabled">停用</option>
          </Select>
        </Field>
        {error ? <p role="alert" className="rounded-lg border border-rust bg-rust-soft px-3 py-2 text-xs text-rust md:col-span-2">{error}</p> : null}
        <div className="flex justify-end gap-2 md:col-span-2">
          <Button type="button" variant="secondary" onClick={onCancel}>取消</Button>
          <Button type="submit" loading={busy}>{initial ? "保存修改" : "接入工具"}</Button>
        </div>
      </form>
    </Card>
  );
}

export default function MCPsPage() {
  const mcps = useRemote(() => requestJSON<Page<MCP>>("/api/v1/mcps"), []);
  const [editing, setEditing] = useState<MCP | null | "new">(null);
  const [deleting, setDeleting] = useState<MCP | null>(null);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const mcpItems = mcps.data?.items ?? [];
  const paging = useClientPage(mcpItems, 10);

  const remove = async () => {
    if (!deleting) {
      return;
    }
    setBusy(true);
    setDeleteError(null);
    try {
      await mutate<void>(`/api/v1/mcps/${deleting.id}`, "DELETE");
      setDeleting(null);
      mcps.reload();
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
        title="外部工具"
        description="外部工具让助手能查资料或操作内部系统。为每个工具标注风险等级，高风险工具执行前会请求审批。"
        actions={<Button onClick={() => setEditing("new")}>接入外部工具</Button>}
      />
      {mcps.error ? <ErrorBanner message={mcps.error} onRetry={mcps.reload} /> : null}
      {editing ? <MCPForm initial={editing === "new" ? null : editing} onCancel={() => setEditing(null)} onSaved={() => { setEditing(null); mcps.reload(); }} /> : null}
      <Card title="已接入工具" description={`共 ${mcpItems.length} 个工具`}>
        {mcps.loading ? (
          <LoadingState />
        ) : mcps.data && mcps.data.items.length > 0 ? (
          <>
          <div className="overflow-x-auto">
            <table className="w-full min-w-[820px] text-left text-sm">
              <thead className="text-xs uppercase tracking-wide text-muted">
                <tr>
                  <th className="pb-3 pr-4 font-medium">名称</th>
                  <th className="pb-3 pr-4 font-medium">地址</th>
                  <th className="pb-3 pr-4 font-medium">鉴权</th>
                  <th className="pb-3 pr-4 font-medium">风险</th>
                  <th className="pb-3 pr-4 font-medium">密钥版本</th>
                  <th className="pb-3 pr-4 font-medium">状态</th>
                  <th className="pb-3 font-medium">操作</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-line">
                {paging.pageItems.map((mcp) => (
                  <tr key={mcp.id}>
                    <td className="py-3 pr-4 font-medium text-ink">{mcp.name}</td>
                    <td className="max-w-[260px] truncate py-3 pr-4 font-mono text-xs text-muted">{mcp.endpoint}</td>
                    <td className="py-3 pr-4 text-ink-soft">{mcp.auth_type}</td>
                    <td className="py-3 pr-4"><Badge tone={mcp.risk_level === "high" ? "danger" : mcp.risk_level === "medium" ? "warning" : "neutral"}>{mcp.risk_level === "high" ? "高风险" : mcp.risk_level === "medium" ? "需留意" : "低风险"}</Badge></td>
                    <td className="py-3 pr-4 text-ink-soft">v{mcp.key_version}</td>
                    <td className="py-3 pr-4"><Badge tone={statusTone(mcp.status)}>{statusLabel(mcp.status)}</Badge></td>
                    <td className="py-3">
                      <div className="flex gap-2">
                        <Button size="sm" variant="secondary" onClick={() => setEditing(mcp)}>编辑</Button>
                        <Button size="sm" variant="danger" onClick={() => { setDeleting(mcp); setDeleteError(null); }}>删除</Button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <Pager page={paging.page} totalPages={paging.totalPages} total={mcpItems.length} onChange={paging.setPage} />
          </>
        ) : (
          <EmptyState title="还没有外部工具" description="接入工具服务后，创建助手时即可勾选使用。" action={<Button size="sm" onClick={() => setEditing("new")}>接入外部工具</Button>} />
        )}
      </Card>
      <ConfirmDialog
        open={deleting !== null}
        title={`删除 MCP ${deleting?.name ?? ""}？`}
        description="被已发布 Agent 引用的 MCP 无法删除。"
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
