// 执行审计入口：按会话浏览 Run 列表并进入步骤级审计。
import { Link, useSearchParams } from "react-router-dom";
import { Badge, Card, EmptyState, ErrorBanner, LoadingState, PageHeader, Select } from "../components/ui";
import { requestJSON } from "../lib/api";
import { formatDateTime, formatDuration, formatRelative, statusLabel, statusTone } from "../lib/format";
import { useRemote } from "../lib/hooks";
import type { Page, Run, Session } from "../lib/types";

export default function AuditPage() {
  const [params, setParams] = useSearchParams();
  const sessionID = params.get("session") ?? "";
  const sessions = useRemote(() => requestJSON<Page<Session>>("/api/v1/sessions?limit=100"), []);
  const runs = useRemote(
    () => (sessionID ? requestJSON<Page<Run>>(`/api/v1/sessions/${sessionID}/runs?limit=100`) : Promise.resolve({ items: [], next_cursor: null })),
    [sessionID],
  );

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
              }}
              aria-label="选择会话"
            >
              <option value="">请选择会话</option>
              {sessions.data.items.map((session) => (
                <option key={session.id} value={session.id}>{session.title}（{formatRelative(session.last_message_at)}）</option>
              ))}
            </Select>
            {!sessionID ? (
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
                    {sessions.data.items.map((session) => (
                      <tr key={session.id}>
                        <td className="py-3 pr-4 text-ink">{session.title}</td>
                        <td className="py-3 pr-4"><Badge tone={statusTone(session.status)}>{statusLabel(session.status)}</Badge></td>
                        <td className="py-3 pr-4 text-ink-soft">{session.source}</td>
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
            ) : null}
          </div>
        ) : (
          <EmptyState title="还没有会话" description="创建并执行会话后，这里会展示审计入口。" />
        )}
      </Card>
      {sessionID ? (
        <Card title="运行列表" description={`会话 ${sessionID.slice(0, 8)} 最近 100 次运行`}>
          {runs.loading ? (
            <LoadingState />
          ) : runs.data && runs.data.items.length > 0 ? (
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
                  {runs.data.items.map((run) => (
                    <tr key={run.id}>
                      <td className="py-3 pr-4 font-mono text-xs text-ink-soft">{run.id.slice(0, 12)}</td>
                      <td className="py-3 pr-4"><Badge tone={statusTone(run.status)}>{statusLabel(run.status)}</Badge></td>
                      <td className="py-3 pr-4 text-ink-soft">{run.mode}</td>
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
          ) : (
            <EmptyState title="该会话还没有 Run" description="在会话中发送消息后即可生成 Run 审计记录。" />
          )}
        </Card>
      ) : null}
    </div>
  );
}
