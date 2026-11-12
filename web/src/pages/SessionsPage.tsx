// 会话列表：创建、打开、归档、导出与异步删除，并提供长期记忆数据操作。
import { useEffect, useRef, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { FormDrawer } from "../components/FormDrawer";
import { Icon } from "../components/Icon";
import { useToast } from "../components/Toast";
import { ActionLink, Badge, Button, Callout, Card, ConfirmDialog, CursorPager, EmptyState, ErrorBanner, Field, Input, LoadingState, MenuItem, PageHeader, RowMenu, Select } from "../components/ui";
import { downloadJSON, mutate, requestJSON } from "../lib/api";
import { describeError } from "../lib/errors";
import { formatDateTime, formatRelative, formatSessionSource, statusLabel, statusTone } from "../lib/format";
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
  const [params, setParams] = useSearchParams();
  const navigate = useNavigate();
  const toast = useToast();
  const preselectedAgent = params.get("agent") ?? "";
  const showCreate = params.get("new") === "1" || preselectedAgent !== "";
  const titleRef = useRef<HTMLInputElement>(null);
  const paging = useCursorStack();
  const sessions = useRemote(
    () => requestJSON<Page<Session>>(`/api/v1/sessions?limit=20${paging.cursor ? `&cursor=${encodeURIComponent(paging.cursor)}` : ""}`),
    [paging.cursor],
  );
  const agents = useRemote(() => requestJSON<Page<Agent>>("/api/v1/agents?limit=100"), []);
  const [query, setQuery] = useState("");
  const [title, setTitle] = useState("新对话");
  const [agentChoice, setAgentChoice] = useState<{ preset: string; id: string } | null>(null);
  const [creating, setCreating] = useState(false);
  const [creatingError, setCreatingError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<Session | null>(null);
  const [deleteStage, setDeleteStage] = useState<string | null>(null);
  const [confirmMemory, setConfirmMemory] = useState(false);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (showCreate && !agents.loading) titleRef.current?.focus();
  }, [showCreate, preselectedAgent, agents.loading]);

  const openCreate = () => {
    const next = new URLSearchParams(params);
    next.set("new", "1");
    setParams(next, { replace: true });
  };

  const resetCreate = () => {
    setTitle("新对话");
    setAgentChoice(null);
    setCreatingError(null);
  };

  const closeCreate = () => {
    const next = new URLSearchParams(params);
    next.delete("agent");
    next.delete("new");
    resetCreate();
    setParams(next, { replace: true });
  };

  const publishedAgents = (agents.data?.items ?? []).filter((agent) => agent.status === "published");
  const defaultAgentID = publishedAgents[0]?.id ?? "";
  // 从助手卡片带入的 agent 可能已下架，回退到第一个可用助手，避免提交无效 ID。
  const agentID = agentChoice?.preset === preselectedAgent ? agentChoice.id : preselectedAgent;
  const selectedAgentID = publishedAgents.some((agent) => agent.id === agentID) ? agentID : defaultAgentID;
  const normalizedQuery = query.trim().toLowerCase();
  const visibleSessions = (sessions.data?.items ?? []).filter((session) =>
    normalizedQuery === "" || session.title.toLowerCase().includes(normalizedQuery),
  );

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
      navigate(`/sessions/${session.id}`, { replace: true });
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
      toast.show(`「${session.title}」已归档`, "success");
    } catch (failure) {
      setActionError(describeError(failure));
      toast.show("归档失败", "danger");
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
      toast.show("导出失败", "danger");
    }
  };

  // 删除请求入队后立即关闭弹窗，清理在后台完成，避免用户盯着进度条等待。
  const deleteSession = async () => {
    if (!deleting) {
      return;
    }
    const target = deleting;
    setBusy(true);
    setActionError(null);
    try {
      const request = await mutate<DeletionRequest>(`/api/v1/sessions/${target.id}/deletion?source=web`, "POST", undefined, crypto.randomUUID());
      setDeleting(null);
      setBusy(false);
      toast.show("删除任务已排队，完成后会自动刷新列表", "info");
      const result = await waitForDeletion(request.id);
      if (result.status !== "completed") {
        throw new Error(result.last_error || "删除任务失败，请稍后重试");
      }
      sessions.reload();
      toast.show(`「${target.title}」已删除`, "success");
    } catch (failure) {
      setActionError(failure instanceof Error && failure.message ? failure.message : describeError(failure));
      toast.show("会话删除失败", "danger");
    } finally {
      setBusy(false);
    }
  };

  const exportMemory = async () => {
    setActionError(null);
    try {
      await downloadJSON("/api/v1/memory/export", "memory-export.json");
      toast.show("记忆已导出", "success");
    } catch (failure) {
      setActionError(describeError(failure));
      toast.show("导出记忆失败", "danger");
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
      toast.show("长期记忆已清空", "success");
    } catch (failure) {
      setActionError(failure instanceof Error && failure.message ? failure.message : describeError(failure));
      toast.show("清除记忆失败", "danger");
    } finally {
      setBusy(false);
      setDeleteStage(null);
    }
  };

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="对话"
        description="继续最近的会话，或选择助手开始新任务。"
        actions={
          <Button onClick={openCreate} aria-haspopup="dialog">
            <Icon name="plus" />
            新建对话
          </Button>
        }
      />
      {sessions.error ? <ErrorBanner message={sessions.error} onRetry={sessions.reload} /> : null}
      {actionError ? <ErrorBanner message={actionError} /> : null}
      {showCreate ? (
        <FormDrawer
          title="新建对话"
          description="选择助手，并为这次会话填写标题。"
          bodyClassName="grid-cols-1"
          submitLabel={!agents.loading && !agents.error && publishedAgents.length ? "开始对话" : undefined}
          submitDisabled={!title.trim()}
          dirty={title !== "新对话" || Boolean(agentChoice && selectedAgentID !== (preselectedAgent || defaultAgentID))}
          busy={creating}
          error={creatingError}
          onSubmit={create}
          onCancel={closeCreate}
          onDiscardNavigation={resetCreate}
        >
          {agents.error ? <ErrorBanner message={agents.error} onRetry={agents.reload} /> : agents.loading ? (
            <LoadingState label="加载可用助手" />
          ) : publishedAgents.length === 0 ? (
            <Callout tone="warning" title="还没有可用助手">
              <Link to="/agents/new" className="text-ochre underline underline-offset-2">创建并发布助手</Link>后即可开始对话。
            </Callout>
          ) : (
            <>
              <Field label="对话标题">
                <Input ref={titleRef} value={title} onChange={(event) => setTitle(event.target.value)} required maxLength={256} placeholder="例如：整理本周需求" />
              </Field>
              <Field label="助手">
                <Select value={selectedAgentID} onChange={(event) => setAgentChoice({ preset: preselectedAgent, id: event.target.value })} required>
                  {publishedAgents.map((agent) => (
                    <option key={agent.id} value={agent.id}>{agent.name}（v{agent.version}）</option>
                  ))}
                </Select>
              </Field>
            </>
          )}
        </FormDrawer>
      ) : null}
      <Card
        title="我的对话"
        actions={
          <div className="relative w-full sm:w-56">
            <Icon name="search" className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted" />
            <Input
              value={query}
              onChange={(event) => setQuery(event.target.value)}
              placeholder="搜索本页标题"
              aria-label="搜索本页会话标题"
              className="pl-9"
            />
          </div>
        }
      >
        {sessions.loading ? (
          <LoadingState />
        ) : sessions.data && sessions.data.items.length > 0 ? (
          <>
            {visibleSessions.length === 0 ? (
              <EmptyState title="没有匹配的会话" action={<Button size="sm" variant="secondary" onClick={() => setQuery("")}>清空搜索</Button>} />
            ) : (
              <ul className="divide-y divide-line">
                {visibleSessions.map((session) => (
                  <li key={session.id} className="flex min-w-0 items-center gap-2 py-2 first:pt-0 sm:gap-4">
                    <Link to={`/sessions/${session.id}`} className="group flex min-w-0 flex-1 items-center gap-3 rounded-lg p-2 transition-colors hover:bg-paper-deep">
                      <span className="grid size-9 shrink-0 place-items-center rounded-lg border border-line bg-paper text-muted group-hover:text-accent"><Icon name="chat" className="size-4" /></span>
                      <span className="min-w-0 flex-1">
                        <span className="block truncate text-sm font-medium group-hover:text-accent">{session.title}</span>
                        <span className="mt-1 block text-xs text-muted">
                          {formatSessionSource(session.source)} · {formatRelative(session.last_message_at ?? session.created_at)}
                          <span className="sm:hidden"> · {statusLabel(session.status)}</span>
                        </span>
                      </span>
                      <span className="hidden shrink-0 sm:block"><Badge tone={statusTone(session.status)}>{statusLabel(session.status)}</Badge></span>
                      <time className="hidden shrink-0 text-xs text-muted lg:block" dateTime={session.created_at}>{formatDateTime(session.created_at)}</time>
                    </Link>
                    <RowMenu name={session.title}>
                      {session.status === "active" ? (
                        <MenuItem disabled={busy} onClick={() => void archive(session)}>归档对话</MenuItem>
                      ) : null}
                      <MenuItem disabled={busy} onClick={() => void exportSession(session)}>导出 JSON</MenuItem>
                      <MenuItem danger disabled={busy} onClick={() => { setDeleting(session); setActionError(null); }}>删除对话</MenuItem>
                    </RowMenu>
                  </li>
                ))}
              </ul>
            )}
            <CursorPager
              page={paging.page}
              count={sessions.data.items.length}
              hasNext={Boolean(sessions.data.next_cursor)}
              onPrev={paging.prev}
              onNext={() => paging.next(sessions.data?.next_cursor ?? null)}
            />
          </>
        ) : sessions.error ? null : (
          <EmptyState title="还没有对话" description="选择助手后即可开始第一段对话。" action={showCreate ? undefined : <Button size="sm" variant="secondary" onClick={openCreate}>新建对话</Button>} />
        )}
      </Card>
      <Card title="隐私与记忆" description="助手会记住跨对话的偏好与事实。你可以随时导出或清空这些记忆；清空不会删除原始对话。">
        <div className="flex flex-wrap gap-2">
          <ActionLink to="/memory" variant="secondary">查看记忆图谱</ActionLink>
          <Button variant="secondary" onClick={() => void exportMemory()}>导出我的记忆</Button>
          <Button variant="danger" onClick={() => setConfirmMemory(true)}>清空长期记忆</Button>
        </div>
      </Card>
      <ConfirmDialog
        open={deleting !== null}
        title={`删除会话 ${deleting?.title ?? ""}？`}
        description="删除后会话不可恢复，关联网关、消息、摘要与向量记忆会被清理。提交后清理在后台进行。"
        confirmLabel="确认删除"
        busy={busy}
        onCancel={() => setDeleting(null)}
        onConfirm={() => void deleteSession()}
      />
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
