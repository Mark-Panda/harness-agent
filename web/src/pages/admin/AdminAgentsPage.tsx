// 管理员全局助手视图：跨用户只读审计，不展示会话内容与配置快照。
import { useState } from "react";
import { Badge, Button, Card, ConfirmDialog, CursorPager, EmptyState, ErrorBanner, LoadingState, PageHeader, Select } from "../../components/ui";
import { mutate, requestJSON } from "../../lib/api";
import { describeError } from "../../lib/errors";
import { formatDateTime, statusLabel, statusTone } from "../../lib/format";
import { useCursorStack, useRemote } from "../../lib/hooks";
import type { AdminAgent, AdminUser, ModelPolicy, Page } from "../../lib/types";

// modeLabel 把技术模式翻译成用户能理解的工作方式。
function modeLabel(mode: string): string {
  switch (mode) {
    case "plan":
      return "先计划再执行";
    case "super":
      return "多助手协作";
    default:
      return "边做边想";
  }
}

export default function AdminAgentsPage() {
  const paging = useCursorStack();
  const [userID, setUserID] = useState("");
  const [status, setStatus] = useState("");
  const [deleting, setDeleting] = useState<AdminAgent | null>(null);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const users = useRemote(() => requestJSON<Page<AdminUser>>("/api/v1/admin/users?limit=100"), []);
  const policies = useRemote(() => requestJSON<Page<ModelPolicy>>("/api/v1/model-policies"), []);
  const agents = useRemote(() => {
    const query = new URLSearchParams({ limit: "20" });
    if (userID) {
      query.set("user_id", userID);
    }
    if (status) {
      query.set("status", status);
    }
    if (paging.cursor) {
      query.set("cursor", paging.cursor);
    }
    return requestJSON<Page<AdminAgent>>(`/api/v1/admin/agents?${query.toString()}`);
  }, [paging.cursor, userID, status]);
  const policyNames = new Map(
    (policies.data?.items ?? []).map((policy) => [policy.id, `${policy.name} v${policy.version}`]),
  );
  const items = agents.data?.items ?? [];

  // 筛选条件变化时重置游标，避免沿用旧条件下的分页位置。
  const changeUser = (value: string) => {
    setUserID(value);
    paging.reset();
  };
  const changeStatus = (value: string) => {
    setStatus(value);
    paging.reset();
  };

  // 管理员跨用户删除助手版本；历史会话与运行记录保留。
  const remove = async () => {
    if (!deleting) {
      return;
    }
    setBusy(true);
    setDeleteError(null);
    try {
      await mutate<void>(`/api/v1/admin/agents/${deleting.id}`, "DELETE");
      setDeleting(null);
      agents.reload();
    } catch (failure) {
      setDeleteError(describeError(failure));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        eyebrow="平台设置 · 审计"
        title="全部助手"
        description="跨用户查看助手的版本、状态与选模规则，仅用于平台运维排查；不展示会话内容与配置快照。"
      />
      {agents.error ? <ErrorBanner message={agents.error} onRetry={agents.reload} /> : null}
      {users.error ? <ErrorBanner message={`用户筛选不可用：${users.error}`} onRetry={users.reload} /> : null}
      <Card
        title="助手列表"
        description={`本页 ${items.length} 条，按创建时间倒序。`}
        actions={
          <div className="flex flex-wrap items-center gap-2">
            <Select className="w-48" value={userID} onChange={(event) => changeUser(event.target.value)} aria-label="按用户筛选">
              <option value="">全部用户</option>
              {(users.data?.items ?? []).map((item) => (
                <option key={item.id} value={item.id}>{item.username}</option>
              ))}
            </Select>
            <Select className="w-36" value={status} onChange={(event) => changeStatus(event.target.value)} aria-label="按状态筛选">
              <option value="">全部状态</option>
              <option value="draft">草稿</option>
              <option value="published">已发布</option>
              <option value="disabled">已停用</option>
              <option value="archived">已归档</option>
            </Select>
          </div>
        }
      >
        {agents.loading ? (
          <LoadingState />
        ) : items.length > 0 ? (
          <>
            <div className="overflow-x-auto">
              <table className="w-full min-w-[900px] text-left text-sm">
                <thead className="text-xs uppercase tracking-wide text-muted">
                  <tr>
                    <th className="pb-3 pr-4 font-medium">助手</th>
                    <th className="pb-3 pr-4 font-medium">用户</th>
                    <th className="pb-3 pr-4 font-medium">工作方式</th>
                    <th className="pb-3 pr-4 font-medium">状态</th>
                    <th className="pb-3 pr-4 font-medium">选模规则</th>
                    <th className="pb-3 pr-4 font-medium">版本</th>
                    <th className="pb-3 pr-4 font-medium">更新时间</th>
                    <th className="pb-3 font-medium">操作</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-line">
                  {items.map((agent) => (
                    <tr key={agent.id}>
                      <td className="py-3 pr-4">
                        <p className="font-medium text-ink">{agent.name}</p>
                        <p className="mt-0.5 max-w-[320px] truncate text-[11px] text-muted">{agent.description || "—"}</p>
                      </td>
                      <td className="py-3 pr-4 font-mono text-xs text-ink-soft">{agent.username || "—"}</td>
                      <td className="py-3 pr-4 text-xs text-ink-soft">{modeLabel(agent.mode)}</td>
                      <td className="py-3 pr-4"><Badge tone={statusTone(agent.status)}>{statusLabel(agent.status)}</Badge></td>
                      <td className="py-3 pr-4 text-xs text-ink-soft">
                        {agent.model_policy_id
                          ? policyNames.get(agent.model_policy_id) ?? `策略 v${agent.model_policy_version}`
                          : "未绑定"}
                      </td>
                      <td className="py-3 pr-4 text-xs text-muted">v{agent.version}</td>
                      <td className="py-3 pr-4 text-xs text-muted">{formatDateTime(agent.updated_at)}</td>
                      <td className="py-3">
                        <Button size="sm" variant="danger" onClick={() => { setDeleting(agent); setDeleteError(null); }}>删除</Button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <CursorPager
              page={paging.page}
              count={items.length}
              hasNext={Boolean(agents.data?.next_cursor)}
              onPrev={paging.prev}
              onNext={() => paging.next(agents.data?.next_cursor ?? null)}
            />
          </>
        ) : (
          <EmptyState title="没有符合条件的助手" description="调整用户或状态筛选后重试。" />
        )}
      </Card>
      <ConfirmDialog
        open={deleting !== null}
        title={`删除助手「${deleting?.name ?? ""}」？`}
        description="将以管理员身份删除该助手版本；历史会话与运行记录保留，但不再关联此助手。删除后无法恢复。"
        confirmLabel="确认删除"
        busy={busy}
        onCancel={() => { setDeleting(null); setDeleteError(null); }}
        onConfirm={() => void remove()}
      >
        {deleteError ? <p role="alert" className="text-xs text-rust">{deleteError}</p> : null}
      </ConfirmDialog>
    </div>
  );
}
