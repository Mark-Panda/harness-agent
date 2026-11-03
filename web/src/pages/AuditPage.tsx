// 执行审计入口：管理员按用户/来源浏览全局运行，普通用户按会话浏览自己的运行。
import { useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { Badge, Card, CursorPager, EmptyState, ErrorBanner, LoadingState, PageHeader, Pager, Select } from "../components/ui";
import { requestJSON } from "../lib/api";
import { useAuth } from "../lib/auth";
import { formatDateTime, formatDuration, formatRelative, statusLabel, statusTone } from "../lib/format";
import { useClientPage, useCursorStack, useRemote } from "../lib/hooks";
import type { AdminRun, AdminUser, Page, Run, Session } from "../lib/types";

// modeLabel 把工作方式翻译成用户语言。
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

// sourceLabel 把会话来源翻译成用户语言。
function sourceLabel(source: string): string {
  switch (source) {
    case "feishu":
      return "飞书";
    case "api":
      return "接口";
    default:
      return "网页";
  }
}

export default function AuditPage() {
  const { user } = useAuth();
  const isAdmin = user?.role === "admin";
  const [params, setParams] = useSearchParams();
  const sessionID = params.get("session") ?? "";

  const sessions = useRemote(() => requestJSON<Page<Session>>("/api/v1/sessions?limit=100"), []);
  const runs = useRemote(
    () => (sessionID ? requestJSON<Page<Run>>(`/api/v1/sessions/${sessionID}/runs?limit=100`) : Promise.resolve({ items: [], next_cursor: null })),
    [sessionID],
  );
  const sessionItems = sessions.data?.items ?? [];
  const sessionPaging = useClientPage(sessionItems, 10);
  const runItems = runs.data?.items ?? [];
  const runPaging = useClientPage(runItems, 10);

  // 管理员全局运行记录：用户与来源筛选走服务端游标分页。
  const adminPaging = useCursorStack();
  const [adminUserID, setAdminUserID] = useState("");
  const [adminSource, setAdminSource] = useState("");
  const adminUsers = useRemote(
    () => (isAdmin ? requestJSON<Page<AdminUser>>("/api/v1/admin/users?limit=100") : Promise.resolve({ items: [], next_cursor: null })),
    [isAdmin],
  );
  const adminRuns = useRemote(() => {
    if (!isAdmin) {
      return Promise.resolve({ items: [], next_cursor: null } as Page<AdminRun>);
    }
    const query = new URLSearchParams({ limit: "10" });
    if (adminUserID) {
      query.set("user_id", adminUserID);
    }
    if (adminSource) {
      query.set("source", adminSource);
    }
    if (adminPaging.cursor) {
      query.set("cursor", adminPaging.cursor);
    }
    return requestJSON<Page<AdminRun>>(`/api/v1/admin/runs?${query.toString()}`);
  }, [isAdmin, adminPaging.cursor, adminUserID, adminSource]);

  if (isAdmin && !sessionID) {
    return (
      <div className="flex flex-col gap-6">
        <PageHeader
          eyebrow="运行记录"
          title="全部用户运行记录"
          description="按用户或来源筛选，查看每一次处理的模型、工具、耗时与失败原因。点击“查看过程”进入步骤级审计。"
        />
        {adminRuns.error ? <ErrorBanner message={adminRuns.error} onRetry={adminRuns.reload} /> : null}
        {adminUsers.error ? <ErrorBanner message={`用户筛选不可用：${adminUsers.error}`} onRetry={adminUsers.reload} /> : null}
        <Card
          title="运行列表"
          description={`本页 ${adminRuns.data?.items.length ?? 0} 条，按创建时间倒序。`}
          actions={
            <div className="flex flex-wrap items-center gap-2">
              <Select
                className="w-48"
                value={adminUserID}
                onChange={(event) => {
                  setAdminUserID(event.target.value);
                  adminPaging.reset();
                }}
                aria-label="按用户筛选"
              >
                <option value="">全部用户</option>
                {adminUsers.data?.items.map((item) => (
                  <option key={item.id} value={item.id}>{item.username}</option>
                ))}
              </Select>
              <Select
                className="w-36"
                value={adminSource}
                onChange={(event) => {
                  setAdminSource(event.target.value);
                  adminPaging.reset();
                }}
                aria-label="按来源筛选"
              >
                <option value="">全部来源</option>
                <option value="web">网页</option>
                <option value="feishu">飞书</option>
                <option value="api">接口</option>
              </Select>
            </div>
          }
        >
          {adminRuns.loading ? (
            <LoadingState />
          ) : adminRuns.data && adminRuns.data.items.length > 0 ? (
            <>
              <div className="overflow-x-auto">
                <table className="w-full min-w-[980px] text-left text-sm">
                  <thead className="text-xs uppercase tracking-wide text-muted">
                    <tr>
                      <th className="pb-3 pr-4 font-medium">用户</th>
                      <th className="pb-3 pr-4 font-medium">会话</th>
                      <th className="pb-3 pr-4 font-medium">来源</th>
                      <th className="pb-3 pr-4 font-medium">状态</th>
                      <th className="pb-3 pr-4 font-medium">工作方式</th>
                      <th className="pb-3 pr-4 font-medium">尝试</th>
                      <th className="pb-3 pr-4 font-medium">耗时</th>
                      <th className="pb-3 pr-4 font-medium">创建时间</th>
                      <th className="pb-3 font-medium">操作</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-line">
                    {adminRuns.data.items.map((run) => (
                      <tr key={run.id}>
                        <td className="py-3 pr-4 font-medium text-ink">{run.username || "—"}</td>
                        <td className="max-w-[220px] truncate py-3 pr-4 text-ink-soft">{run.session_title || run.session_id.slice(0, 8)}</td>
                        <td className="py-3 pr-4"><Badge tone={run.source === "feishu" ? "info" : "neutral"}>{sourceLabel(run.source)}</Badge></td>
                        <td className="py-3 pr-4"><Badge tone={statusTone(run.status)}>{statusLabel(run.status)}</Badge></td>
                        <td className="py-3 pr-4 text-ink-soft">{modeLabel(run.mode)}</td>
                        <td className="py-3 pr-4 text-ink-soft">{run.attempt_no}</td>
                        <td className="py-3 pr-4 text-xs text-muted">{formatDuration(run.started_at, run.finished_at)}</td>
                        <td className="py-3 pr-4 text-xs text-muted">{formatDateTime(run.created_at)}</td>
                        <td className="py-3">
                          <Link to={`/runs/${run.id}`} className="text-xs text-accent underline underline-offset-2">查看过程</Link>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              <CursorPager
                page={adminPaging.page}
                count={adminRuns.data.items.length}
                hasNext={Boolean(adminRuns.data.next_cursor)}
                onPrev={adminPaging.prev}
                onNext={() => adminPaging.next(adminRuns.data?.next_cursor ?? null)}
              />
            </>
          ) : (
            <EmptyState title="没有匹配的运行记录" description="调整用户或来源筛选；也可以先让助手执行一次任务。" />
          )}
        </Card>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        eyebrow="运行记录"
        title="每一步都看得见"
        description="选一个对话，查看助手每次处理的完整过程：用了哪个模型、调用了什么工具、哪里失败、花了多少 Token。"
      />
      {sessions.error ? <ErrorBanner message={sessions.error} onRetry={sessions.reload} /> : null}
      {runs.error ? <ErrorBanner message={runs.error} onRetry={runs.reload} /> : null}
      <Card title="选择要查看的对话" description="按最近消息排序。选中后展示这个对话里的每一次运行。">
        {sessions.loading ? (
          <LoadingState />
        ) : sessions.data && sessions.data.items.length > 0 ? (
          <div className="flex flex-col gap-3">
            <Select
              className="max-w-xl"
              value={sessionID}
              onChange={(event) => {
                if (event.target.value) {
                  setParams({ session: event.target.value });
                } else {
                  setParams({});
                }
                runPaging.reset();
              }}
              aria-label="选择会话"
            >
              <option value="">请选择会话</option>
              {sessions.data.items.map((session) => (
                <option key={session.id} value={session.id}>{session.title}（{formatRelative(session.last_message_at)}）</option>
              ))}
            </Select>
            {!sessionID ? (
              <>
                <div className="overflow-x-auto">
                  <table className="w-full min-w-[720px] text-left text-sm">
                    <thead className="text-xs uppercase tracking-wide text-muted">
                      <tr>
                        <th className="pb-3 pr-4 font-medium">会话</th>
                        <th className="pb-3 pr-4 font-medium">状态</th>
                        <th className="pb-3 pr-4 font-medium">来源</th>
                        <th className="pb-3 pr-4 font-medium">最近消息</th>
                        <th className="pb-3 font-medium">操作</th>
                      </tr>
                    </thead>
                    <tbody className="divide-y divide-line">
                      {sessionPaging.pageItems.map((session) => (
                        <tr key={session.id}>
                          <td className="py-3 pr-4 text-ink">{session.title}</td>
                          <td className="py-3 pr-4"><Badge tone={statusTone(session.status)}>{statusLabel(session.status)}</Badge></td>
                          <td className="py-3 pr-4 text-ink-soft">{sourceLabel(session.source)}</td>
                          <td className="py-3 pr-4 text-xs text-muted">{formatRelative(session.last_message_at)}</td>
                          <td className="py-3">
                            <div className="flex gap-2">
                              <Link to={`/audit?session=${session.id}`} className="text-xs text-accent underline underline-offset-2">查看运行</Link>
                              <Link to={`/sessions/${session.id}`} className="text-xs text-muted underline underline-offset-2">打开对话</Link>
                            </div>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
                <Pager page={sessionPaging.page} totalPages={sessionPaging.totalPages} total={sessionItems.length} onChange={sessionPaging.setPage} />
              </>
            ) : null}
          </div>
        ) : (
          <EmptyState title="还没有会话" description="创建并执行会话后，这里会展示审计入口。" />
        )}
      </Card>
      {sessionID ? (
        <Card title="运行列表" description={`会话 ${sessionID.slice(0, 8)} 的运行记录`}>
          {runs.loading ? (
            <LoadingState />
          ) : runs.data && runs.data.items.length > 0 ? (
            <>
              <div className="overflow-x-auto">
                <table className="w-full min-w-[900px] text-left text-sm">
                  <thead className="text-xs uppercase tracking-wide text-muted">
                    <tr>
                      <th className="pb-3 pr-4 font-medium">运行</th>
                      <th className="pb-3 pr-4 font-medium">状态</th>
                      <th className="pb-3 pr-4 font-medium">工作方式</th>
                      <th className="pb-3 pr-4 font-medium">尝试次数</th>
                      <th className="pb-3 pr-4 font-medium">耗时</th>
                      <th className="pb-3 pr-4 font-medium">创建时间</th>
                      <th className="pb-3 font-medium">操作</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-line">
                    {runPaging.pageItems.map((run) => (
                      <tr key={run.id}>
                        <td className="py-3 pr-4 font-mono text-xs text-ink-soft">{run.id.slice(0, 12)}</td>
                        <td className="py-3 pr-4"><Badge tone={statusTone(run.status)}>{statusLabel(run.status)}</Badge></td>
                        <td className="py-3 pr-4 text-ink-soft">{modeLabel(run.mode)}</td>
                        <td className="py-3 pr-4 text-ink-soft">{run.attempt_no}</td>
                        <td className="py-3 pr-4 text-xs text-muted">{formatDuration(run.started_at, run.finished_at)}</td>
                        <td className="py-3 pr-4 text-xs text-muted">{formatDateTime(run.created_at)}</td>
                        <td className="py-3">
                          <Link to={`/runs/${run.id}`} className="text-xs text-accent underline underline-offset-2">查看过程</Link>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              <Pager page={runPaging.page} totalPages={runPaging.totalPages} total={runItems.length} onChange={runPaging.setPage} />
            </>
          ) : (
            <EmptyState title="该会话还没有 Run" description="在会话中发送消息后即可生成 Run 审计记录。" />
          )}
        </Card>
      ) : null}
    </div>
  );
}
