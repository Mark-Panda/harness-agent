// 会话列表：创建、打开、归档、导出与异步删除，并提供长期记忆数据操作。
import { useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { Badge, Button, Callout, Card, ConfirmDialog, CursorPager, DropdownMenu, EmptyState, ErrorBanner, Field, Input, LoadingState, MenuItem, PageHeader, Select } from "../components/ui";
import { downloadJSON, mutate, requestJSON } from "../lib/api";
import { describeError } from "../lib/errors";
import { formatDateTime, formatRelative, statusLabel, statusTone } from "../lib/format";
import { useCursorStack, useRemote } from "../lib/hooks";
import type { Agent, DeletionRequest, Page, Session } from "../lib/types";

// waitForDeletion 轮询删除请求直到终态或超时。
async function waitForDeletion(id: string): Promise<DeletionRequest> {
  let latest: DeletionRequest | null = null;
  for (let attempt = 0; attempt < 20; attempt += 1) {
    latest = await requestJSON<DeletionRequest>(`/api/v1/deletions/${id}`);
    if (latest.status === "completed" || latest.status === "failed") {
      return latest;
    }
    await new Promise((resolve) => window.setTimeout(resolve, 1000));
  }
  if (!latest) {
    throw new Error("删除请求状态不可用");
  }
  return latest;
}

export default function SessionsPage() {
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const preselectedAgent = params.get("agent") ?? "";
  const paging = useCursorStack();
  const sessions = useRemote(
    () => requestJSON<Page<Session>>(`/api/v1/sessions?limit=20${paging.cursor ? `&cursor=${encodeURIComponent(paging.cursor)}` : ""}`),
    [paging.cursor],
  );
  const agents = useRemote(() => requestJSON<Page<Agent>>("/api/v1/agents?limit=100"), []);
  const [title, setTitle] = useState("新对话");
  const [agentID, setAgentID] = useState(preselectedAgent);
  const [creating, setCreating] = useState(false);
  const [creatingError, setCreatingError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<Session | null>(null);
  const [deleteStage, setDeleteStage] = useState<string | null>(null);
  const [confirmMemory, setConfirmMemory] = useState(false);
  const [busy, setBusy] = useState(false);

  const publishedAgents = (agents.data?.items ?? []).filter((agent) => agent.status === "published");
  const defaultAgentID = publishedAgents[0]?.id ?? "";
  // 从助手卡片带入的 agent 可能已下架，回退到第一个可用助手，避免提交无效 ID。
  const selectedAgentID = publishedAgents.some((agent) => agent.id === agentID) ? agentID : defaultAgentID;

  const create = async (event: React.FormEvent) => {
    event.preventDefault();
    setCreating(true);
    setCreatingError(null);
    try {
      if (!selectedAgentID) {
        throw new Error("请先在助手页创建并发布一个助手");
      }
      const session = await mutate<Session>("/api/v1/sessions", "POST", {
        agent_id: selectedAgentID,
        title: title.trim(),
        source: "web",
      }, crypto.randomUUID());
      navigate(`/sessions/${session.id}`);
    } catch (failure) {
      setCreatingError(failure instanceof Error && failure.message === "请先在助手页创建并发布一个助手" ? failure.message : describeError(failure));
    } finally {
      setCreating(false);
    }
  };

  const archive = async (session: Session) => {
    setBusy(true);
    setActionError(null);
    try {
      await mutate<Session>(`/api/v1/sessions/${session.id}/archive`, "POST", undefined, crypto.randomUUID());
      sessions.reload();
    } catch (failure) {
      setActionError(describeError(failure));
    } finally {
      setBusy(false);
    }
  };

  const exportSession = async (session: Session) => {
    setActionError(null);
    try {
      await downloadJSON(`/api/v1/sessions/${session.id}/export`, `session-${session.id}.json`);
    } catch (failure) {
      setActionError(describeError(failure));
    }
  };

  const deleteSession = async () => {
    if (!deleting) {
      return;
    }
    setBusy(true);
    setActionError(null);
    setDeleteStage("正在创建删除请求…");
    try {
      const request = await mutate<DeletionRequest>(`/api/v1/sessions/${deleting.id}/deletion?source=web`, "POST", undefined, crypto.randomUUID());
      setDeleteStage("删除任务已排队，等待清理完成…");
      const result = await waitForDeletion(request.id);
      if (result.status !== "completed") {
        throw new Error(result.last_error || "删除任务失败，请稍后重试");
      }
      setDeleting(null);
      sessions.reload();
    } catch (failure) {
      setActionError(failure instanceof Error && failure.message ? failure.message : describeError(failure));
    } finally {
      setBusy(false);
      setDeleteStage(null);
    }
  };

  const exportMemory = async () => {
    setActionError(null);
    try {
      await downloadJSON("/api/v1/memory/export", "memory-export.json");
    } catch (failure) {
      setActionError(describeError(failure));
    }
  };

  const deleteMemory = async () => {
    setBusy(true);
    setActionError(null);
    setDeleteStage("正在删除长期记忆…");
    try {
      const request = await mutate<DeletionRequest>("/api/v1/memory/deletion?source=web", "POST", undefined, crypto.randomUUID());
      const result = await waitForDeletion(request.id);
      if (result.status !== "completed") {
        throw new Error(result.last_error || "记忆删除失败，请稍后重试");
      }
      setConfirmMemory(false);
    } catch (failure) {
      setActionError(failure instanceof Error && failure.message ? failure.message : describeError(failure));
    } finally {
      setBusy(false);
      setDeleteStage(null);
    }
  };

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        eyebrow="对话"
        title="对话"
        description="选一个助手，像和同事说话一样描述目标。每次发送都会生成一条可回看的运行记录。"
        actions={
          <Button variant="secondary" onClick={() => void exportMemory()}>
            导出我的记忆
          </Button>
        }
      />
      {sessions.error ? <ErrorBanner message={sessions.error} onRetry={sessions.reload} /> : null}
      {actionError ? <ErrorBanner message={actionError} /> : null}
      {publishedAgents.length === 0 ? (
        <Callout tone="warning" title="还没有可用助手">
          先到 <Link to="/agents/new" className="text-accent underline underline-offset-2">助手页创建并发布一个助手</Link>，然后回到这里开始对话。
        </Callout>
      ) : null}
      <Card title="开始新对话" description="给这次对话起个标题，方便以后在列表中找到它。">
        <form onSubmit={create} className="grid gap-4 md:grid-cols-[1fr_1fr_auto] md:items-end">
          <Field label="这次想聊什么？" hint="之后也可以修改标题。">
            <Input value={title} onChange={(event) => setTitle(event.target.value)} required maxLength={256} placeholder="例如：整理本周需求" />
          </Field>
          <Field label="使用哪个助手" hint={publishedAgents.length === 0 ? "请先在助手页创建并发布一个助手。" : "只列出已发布版本。"}>
            <Select value={selectedAgentID} onChange={(event) => setAgentID(event.target.value)} required>
              {publishedAgents.map((agent) => (
                <option key={agent.id} value={agent.id}>{agent.name}（v{agent.version}）</option>
              ))}
            </Select>
          </Field>
          <Button type="submit" loading={creating} disabled={publishedAgents.length === 0}>开始对话</Button>
        </form>
        {creatingError ? <p role="alert" className="mt-3 text-xs text-rust">{creatingError}</p> : null}
      </Card>
      <Card title="我的对话" description={`本页 ${sessions.data?.items.length ?? 0} 个会话，按最近更新排序。`}>
        {sessions.loading ? (
          <LoadingState />
        ) : sessions.data && sessions.data.items.length > 0 ? (
          <>
          <div className="overflow-x-auto">
            <table className="w-full min-w-[860px] text-left text-sm">
              <thead className="text-xs uppercase tracking-wide text-muted">
                <tr>
                  <th className="pb-3 pr-4 font-medium">标题</th>
                  <th className="pb-3 pr-4 font-medium">状态</th>
                  <th className="pb-3 pr-4 font-medium">来源</th>
                  <th className="pb-3 pr-4 font-medium">最近消息</th>
                  <th className="pb-3 pr-4 font-medium">创建时间</th>
                  <th className="pb-3 text-right font-medium">操作</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-line">
                {sessions.data.items.map((session) => (
                  <tr key={session.id}>
                    <td className="py-3 pr-4">
                      <Link to={`/sessions/${session.id}`} className="font-medium text-ink underline-offset-2 hover:text-jade hover:underline">
                        {session.title}
                      </Link>
                      <p className="font-mono text-[11px] text-muted">{session.id.slice(0, 8)}</p>
                    </td>
                    <td className="py-3 pr-4"><Badge tone={statusTone(session.status)}>{statusLabel(session.status)}</Badge></td>
                    <td className="py-3 pr-4 text-ink-soft">{session.source}</td>
                    <td className="py-3 pr-4 text-xs text-muted">{formatRelative(session.last_message_at)}</td>
                    <td className="py-3 pr-4 text-xs text-muted">{formatDateTime(session.created_at)}</td>
                    <td className="py-3 text-right">
                      <DropdownMenu
                        label={`更多操作：${session.title}`}
                        triggerLabel="更多操作"
                        trigger={<span aria-hidden className="font-display text-base leading-none">⋯</span>}
                        triggerClassName="w-8 border border-line-strong bg-paper-raised text-base text-ink hover:border-accent"
                        panelClassName="w-40"
                      >
                        {session.status === "active" ? (
                          <MenuItem disabled={busy} onClick={() => void archive(session)}>归档对话</MenuItem>
                        ) : null}
                        <MenuItem disabled={busy} onClick={() => void exportSession(session)}>导出 JSON</MenuItem>
                        <MenuItem
                          danger
                          disabled={busy}
                          onClick={() => {
                            setDeleting(session);
                            setActionError(null);
                          }}
                        >
                          删除对话
                        </MenuItem>
                      </DropdownMenu>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <CursorPager
            page={paging.page}
            count={sessions.data.items.length}
            hasNext={Boolean(sessions.data.next_cursor)}
            onPrev={paging.prev}
            onNext={() => paging.next(sessions.data?.next_cursor ?? null)}
          />
          </>
        ) : (
          <EmptyState title="还没有对话" description="创建第一个对话后，就可以和助手交流并查看处理过程。" />
        )}
      </Card>
      <Card title="隐私与记忆" description="助手会记住跨对话的偏好与事实。你可以随时导出或清空这些记忆；清空不会删除原始对话。">
        <div className="flex flex-wrap gap-2">
          <Button variant="secondary" onClick={() => void exportMemory()}>导出我的记忆</Button>
          <Button variant="danger" onClick={() => setConfirmMemory(true)}>清空长期记忆</Button>
        </div>
      </Card>
      <ConfirmDialog
        open={deleting !== null}
        title={`删除会话 ${deleting?.title ?? ""}？`}
        description="删除后会话不可恢复，关联网关、消息、摘要与向量记忆会被清理。"
        confirmLabel="确认删除"
        busy={busy}
        onCancel={() => setDeleting(null)}
        onConfirm={() => void deleteSession()}
      >
        {deleteStage ? <p role="status" className="text-xs text-muted">{deleteStage}</p> : null}
      </ConfirmDialog>
      <ConfirmDialog
        open={confirmMemory}
        title="删除全部长期记忆？"
        description="该操作会清空当前用户的向量记忆与结构化事实，不影响会话原始消息。"
        confirmLabel="确认删除记忆"
        busy={busy}
        onCancel={() => setConfirmMemory(false)}
        onConfirm={() => void deleteMemory()}
      >
        {deleteStage ? <p role="status" className="text-xs text-muted">{deleteStage}</p> : null}
      </ConfirmDialog>
    </div>
  );
}
