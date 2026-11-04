// MCP 接入管理：仅允许 HTTPS 公网端点，凭证只写不读，风险等级决定审批策略。
import { useState } from "react";
import { Badge, Button, Card, ConfirmDialog, EmptyState, ErrorBanner, Field, Input, LoadingState, PageHeader, Pager, Select, Textarea } from "../../components/ui";
import { mutate, requestJSON } from "../../lib/api";
import { describeError } from "../../lib/errors";
import { formatDateTime, statusLabel, statusTone } from "../../lib/format";
import { useClientPage, useRemote, useScrollToTop } from "../../lib/hooks";
import type { MCP, MCPInput, MCPStatus, MCPTool, Page } from "../../lib/types";

interface MCPFormState {
  name: string;
  endpoint: string;
  authType: "none" | "bearer" | "oauth" | "header";
  credentials: string;
  riskLevel: "low" | "medium" | "high";
  status: MCPStatus;
}

const emptyMCP: MCPFormState = { name: "", endpoint: "", authType: "none", credentials: "", riskLevel: "low", status: "enabled" };

// inheritedTimeout 按最长前缀匹配已保存的超时，与服务端执行层的匹配规则保持一致。
function inheritedTimeout(overrides: Record<string, number>, toolName: string): { prefix: string; seconds: number } | null {
  let matched: { prefix: string; seconds: number } | null = null;
  for (const [prefix, seconds] of Object.entries(overrides ?? {})) {
    if (seconds <= 0 || !toolName.startsWith(prefix)) {
      continue;
    }
    if (!matched || prefix.length > matched.prefix.length) {
      matched = { prefix, seconds };
    }
  }
  return matched;
}

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
      const body: MCPInput = {
        name: form.name.trim(),
        endpoint: form.endpoint.trim(),
        auth_type: form.authType,
        credentials: form.credentials,
        risk_level: form.riskLevel,
        status: form.status,
        tool_timeouts: initial?.tool_timeouts ?? {},
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
    <Card title={initial ? `编辑外部工具：${initial.name}` : "接入外部工具"} description="外部工具让助手能查询或操作其他系统。默认只允许 HTTPS 公网地址；内网地址需由服务端显式放行。">
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
  useScrollToTop(editing !== null);
  const [deleting, setDeleting] = useState<MCP | null>(null);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [testingID, setTestingID] = useState<string | null>(null);
  const [testResults, setTestResults] = useState<Record<string, { ok: boolean; message: string; tool_count: number }>>({});
  const [toolsFor, setToolsFor] = useState<MCP | null>(null);
  const [tools, setTools] = useState<MCPTool[]>([]);
  const [toolsMessage, setToolsMessage] = useState<string | null>(null);
  const [toolsBusy, setToolsBusy] = useState(false);
  // timeouts 保存界面上按原始工具名填写的超时秒数；留空表示跟随默认。
  const [timeouts, setTimeouts] = useState<Record<string, string>>({});
  const [timeoutBusy, setTimeoutBusy] = useState(false);
  const [timeoutError, setTimeoutError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const mcpItems = mcps.data?.items ?? [];
  const paging = useClientPage(mcpItems, 10);

  // 测试连通：服务端解密凭证后握手 tools/list，不在前端暴露密钥。
  const testMCP = async (mcp: MCP) => {
    setTestingID(mcp.id);
    try {
      const result = await mutate<{ ok: boolean; message: string; tool_count: number }>(
        `/api/v1/mcps/${mcp.id}/test`,
        "POST",
        undefined,
        crypto.randomUUID(),
      );
      setTestResults((current) => ({ ...current, [mcp.id]: result }));
    } catch (failure) {
      setTestResults((current) => ({ ...current, [mcp.id]: { ok: false, message: describeError(failure), tool_count: 0 } }));
    } finally {
      setTestingID(null);
    }
  };

  // 查看工具：加载服务端返回的工具名、说明与参数 Schema。
  const openTools = async (mcp: MCP) => {
    setToolsFor(mcp);
    setToolsBusy(true);
    setToolsMessage(null);
    setTools([]);
    setTimeouts({});
    setTimeoutError(null);
    try {
      const result = await requestJSON<{ ok: boolean; message: string; items: MCPTool[]; count: number }>(`/api/v1/mcps/${mcp.id}/tools`);
      const items = result.items ?? [];
      // 仅把精确工具名的覆盖回填到输入框，前缀覆盖以提示形式展示，避免保存时被展开成大量键。
      const exact: Record<string, string> = {};
      for (const tool of items) {
        const seconds = mcp.tool_timeouts?.[tool.name];
        if (seconds) {
          exact[tool.name] = String(seconds);
        }
      }
      setTimeouts(exact);
      setTools(items);
      setToolsMessage(result.message);
      if (!result.ok) {
        setTools([]);
      }
    } catch (failure) {
      setToolsMessage(describeError(failure));
    } finally {
      setToolsBusy(false);
    }
  };

  // persistTimeouts 保存工具超时覆盖；未填写的工具回退到默认或已配置的前缀覆盖。
  const persistTimeouts = async (next: Record<string, number>) => {
    if (!toolsFor) {
      return;
    }
    setTimeoutBusy(true);
    setTimeoutError(null);
    try {
      const updated = await mutate<MCP>(
        `/api/v1/mcps/${toolsFor.id}`,
        "PUT",
        {
          name: toolsFor.name,
          endpoint: toolsFor.endpoint,
          auth_type: toolsFor.auth_type,
          credentials: "",
          risk_level: toolsFor.risk_level,
          status: toolsFor.status,
          tool_timeouts: next,
        } satisfies MCPInput,
        crypto.randomUUID(),
      );
      setToolsFor(updated);
      mcps.reload();
    } catch (failure) {
      setTimeoutError(describeError(failure));
    } finally {
      setTimeoutBusy(false);
    }
  };

  const saveTimeouts = async () => {
    if (!toolsFor) {
      return;
    }
    // 以已保存的配置为基线，只改动精确工具名对应的键。
    const next: Record<string, number> = { ...(toolsFor.tool_timeouts ?? {}) };
    for (const tool of tools) {
      delete next[tool.name];
      const raw = (timeouts[tool.name] ?? "").trim();
      if (!raw) {
        continue;
      }
      const seconds = Number(raw);
      if (!Number.isInteger(seconds) || seconds < 1 || seconds > 300) {
        setTimeoutError(`工具 ${tool.name} 的超时必须是 1-300 之间的整数秒。`);
        return;
      }
      next[tool.name] = seconds;
    }
    await persistTimeouts(next);
  };

  const resetTimeouts = async () => {
    setTimeouts({});
    await persistTimeouts({});
  };

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
      {toolsFor ? (
        <Card
          title={`${toolsFor.name} 的工具能力`}
          description={toolsMessage ?? "正在读取工具列表…"}
          actions={
            <>
              <Button size="sm" variant="secondary" loading={timeoutBusy} onClick={() => void resetTimeouts()}>全部恢复默认</Button>
              <Button size="sm" loading={timeoutBusy} onClick={() => void saveTimeouts()}>保存超时设置</Button>
              <Button size="sm" variant="secondary" onClick={() => setToolsFor(null)}>关闭</Button>
            </>
          }
        >
          {timeoutError ? <p role="alert" className="mb-3 rounded-lg border border-rust bg-rust-soft px-3 py-2 text-xs text-rust">{timeoutError}</p> : null}
          {toolsBusy ? (
            <LoadingState />
          ) : tools.length > 0 ? (
            <div className="overflow-x-auto">
              <table className="w-full min-w-[900px] text-left text-sm">
                <thead className="text-xs uppercase tracking-wide text-muted">
                  <tr>
                    <th className="pb-3 pr-4 font-medium">工具名</th>
                    <th className="pb-3 pr-4 font-medium">说明</th>
                    <th className="pb-3 pr-4 font-medium">超时（秒）</th>
                    <th className="pb-3 font-medium">参数 Schema</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-line">
                  {tools.map((tool) => {
                    const inherited = inheritedTimeout(toolsFor.tool_timeouts ?? {}, tool.name);
                    return (
                      <tr key={tool.name} className="align-top">
                        <td className="py-3 pr-4 font-mono text-xs text-ink">{tool.name}</td>
                        <td className="py-3 pr-4 text-xs text-ink-soft">{tool.description || "—"}</td>
                        <td className="py-3 pr-4">
                          <Input
                            type="number"
                            min={1}
                            max={300}
                            className="w-24"
                            aria-label={`${tool.name} 超时秒数`}
                            value={timeouts[tool.name] ?? ""}
                            placeholder={inherited ? String(inherited.seconds) : "默认"}
                            onChange={(event) => setTimeouts({ ...timeouts, [tool.name]: event.target.value })}
                          />
                          <p className="mt-1 text-[11px] text-muted">
                            {inherited ? `继承前缀 ${inherited.prefix}：${inherited.seconds} 秒` : "留空使用默认 60 秒"}
                          </p>
                        </td>
                        <td className="py-3 text-xs text-muted">
                          <details>
                            <summary className="cursor-pointer hover:text-accent">查看参数</summary>
                            <pre className="mt-2 max-h-48 overflow-auto rounded-[3px] border border-line bg-paper-deep/40 p-2 font-mono text-[11px] text-ink-soft">
                              {JSON.stringify(tool.input_schema ?? {}, null, 2)}
                            </pre>
                          </details>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          ) : (
            <EmptyState title="没有可用工具" description={toolsMessage ?? "服务端没有返回工具列表。"} />
          )}
        </Card>
      ) : null}
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
                    <td className="py-3 pr-4 font-medium text-ink">
                      {mcp.name}
                      {testResults[mcp.id] ? (
                        <p role="status" className={`mt-1 text-[11px] font-normal ${testResults[mcp.id].ok ? "text-accent" : "text-rust"}`}>
                          {testResults[mcp.id].message}
                        </p>
                      ) : null}
                    </td>
                    <td className="max-w-[260px] truncate py-3 pr-4 font-mono text-xs text-muted">{mcp.endpoint}</td>
                    <td className="py-3 pr-4 text-ink-soft">{mcp.auth_type}</td>
                    <td className="py-3 pr-4"><Badge tone={mcp.risk_level === "high" ? "danger" : mcp.risk_level === "medium" ? "warning" : "neutral"}>{mcp.risk_level === "high" ? "高风险" : mcp.risk_level === "medium" ? "需留意" : "低风险"}</Badge></td>
                    <td className="py-3 pr-4 text-ink-soft">v{mcp.key_version}</td>
                    <td className="py-3 pr-4"><Badge tone={statusTone(mcp.status)}>{statusLabel(mcp.status)}</Badge></td>
                    <td className="py-3">
                      <div className="flex flex-wrap gap-2">
                        <Button size="sm" variant="secondary" loading={testingID === mcp.id} onClick={() => void testMCP(mcp)}>测试连通</Button>
                        <Button size="sm" variant="secondary" onClick={() => void openTools(mcp)}>查看工具</Button>
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
