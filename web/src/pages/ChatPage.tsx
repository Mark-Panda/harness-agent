// 会话对话：发送消息、创建 Run、消费 SSE 增量、审批工具并展示执行时间线。
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { Link, useParams } from "react-router-dom";
import { SafeMarkdown } from "../components/SafeMarkdown";
import { useToast } from "../components/Toast";
import { Badge, Button, Card, EmptyState, ErrorBanner, LoadingState, PageHeader, Textarea, cn } from "../components/ui";
import { downloadAttachment, mutate, requestJSON, streamRunEvents, uploadAttachment } from "../lib/api";
import { describeError } from "../lib/errors";
import { formatDateTime, formatRelative, statusLabel, statusTone } from "../lib/format";
import { useRemote } from "../lib/hooks";
import type { Agent, Attachment, Message, Page, Run, RunEvent, Session, ToolApproval } from "../lib/types";

// terminalStatuses 是 Run 已停止自动执行的状态集合；人工对账虽非终态也不能继续等待流式输出。
const terminalStatuses = new Set(["completed", "failed", "cancelled", "dead", "reconciliation_required"]);

// isTerminal 判断 Run 是否已结束。
function isTerminal(status: string): boolean {
  return terminalStatuses.has(status);
}

// modeLabel 把工作方式翻译成用户语言。
function modeLabel(mode: string | undefined): string {
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
function sourceLabel(source: string | undefined): string {
  switch (source) {
    case "feishu":
      return "飞书";
    case "api":
      return "接口";
    default:
      return "网页";
  }
}

// asString 安全提取事件载荷中的字符串字段。
function asString(value: unknown): string | null {
  return typeof value === "string" && value.length > 0 ? value : null;
}

// toolCallNames 从消息的工具调用记录中提取名称，用于气泡内展示。
function toolCallNames(value: unknown): string[] {
  if (!Array.isArray(value)) {
    return [];
  }
  return value.flatMap((item) => {
    if (!item || typeof item !== "object") {
      return [];
    }
    const name = (item as Record<string, unknown>).name;
    return typeof name === "string" && name.length > 0 ? [name] : [];
  });
}

// approvalFields 把审批参数转成可读的键值对，避免整段 JSON 难以扫读。
function approvalFields(value: Record<string, unknown> | null): Array<[string, string]> {
  if (!value) {
    return [];
  }
  return Object.entries(value).map(([key, item]) => {
    const text = typeof item === "string" ? item : JSON.stringify(item);
    return [key, text ?? String(item)];
  });
}

interface LiveMessage {
  // key 使用事件序号去重。
  key: string;
  role: string;
  content: string;
  toolCalls?: unknown;
  toolCallID?: string;
  name?: string;
}

// PendingAttachment 表示输入区中尚未随消息发送的附件。
interface PendingAttachment {
  // key 本地去重标识。
  key: string;
  // name 文件名。
  name: string;
  // size 文件字节数。
  size: number;
  // status 上传状态：uploading/ready/failed。
  status: "uploading" | "ready" | "failed";
  // id 上传成功后的附件 ID。
  id?: string;
  // error 上传失败原因。
  error?: string;
}

// 附件限制与服务端保持一致：单文件 20MB，每条消息最多 5 个。
const MAX_ATTACHMENT_BYTES = 20 * 1024 * 1024;
const MAX_ATTACHMENTS_PER_MESSAGE = 5;

// formatFileSize 把字节数转换为易读大小。
function formatFileSize(bytes: number): string {
  if (bytes < 1024) {
    return `${bytes} B`;
  }
  if (bytes < 1024 * 1024) {
    return `${(bytes / 1024).toFixed(1)} KB`;
  }
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

interface TimelineEntry {
  key: string;
  label: string;
  detail?: string;
  tone: "info" | "success" | "danger" | "warning";
}

// sleep 支持取消的等待。
function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const timer = window.setTimeout(resolve, ms);
    signal.addEventListener("abort", () => {
      window.clearTimeout(timer);
      resolve();
    }, { once: true });
  });
}

export default function ChatPage() {
  const { sessionId = "" } = useParams();
  const toast = useToast();
  const session = useRemote(() => requestJSON<Session>(`/api/v1/sessions/${sessionId}`), [sessionId]);
  const messages = useRemote(() => requestJSON<Page<Message>>(`/api/v1/sessions/${sessionId}/messages?limit=100`), [sessionId]);
  // 保留已归档助手，仅用于展示会话绑定的助手名称；发送能力仍要求已发布状态。
  const agents = useRemote(() => requestJSON<Page<Agent>>("/api/v1/agents?limit=100&include_archived=true"), []);
  const messagesReload = messages.reload;
  // 长会话按游标继续加载更早消息，避免只显示最近 100 条。
  const [olderMessages, setOlderMessages] = useState<Message[]>([]);
  const [olderCursor, setOlderCursor] = useState<string | null>(null);
  const [olderEnded, setOlderEnded] = useState(false);
  const [olderLoading, setOlderLoading] = useState(false);
  useEffect(() => {
    setOlderMessages([]);
    setOlderCursor(null);
    setOlderEnded(false);
  }, [sessionId]);

  const [runsVersion, setRunsVersion] = useState(0);
  const runs = useRemote(() => requestJSON<Page<Run>>(`/api/v1/sessions/${sessionId}/runs?limit=20`), [sessionId, runsVersion]);

  const [draft, setDraft] = useState("");
  const [sending, setSending] = useState(false);
  const [sendError, setSendError] = useState<string | null>(null);
  const [pendingAttachments, setPendingAttachments] = useState<PendingAttachment[]>([]);
  const fileInputRef = useRef<HTMLInputElement | null>(null);
  // removedAttachmentKeys 记录上传完成前已被用户移除的文件，用于回收服务端对象。
  const removedAttachmentKeys = useRef<Set<string>>(new Set());
  const [activeRun, setActiveRun] = useState<Run | null>(null);
  const [liveMessages, setLiveMessages] = useState<LiveMessage[]>([]);
  const [timeline, setTimeline] = useState<TimelineEntry[]>([]);
  const [connection, setConnection] = useState<"idle" | "connecting" | "streaming" | "reconnecting" | "closed">("idle");
  const [streamError, setStreamError] = useState<string | null>(null);
  const [approvals, setApprovals] = useState<ToolApproval[]>([]);
  const [deciding, setDeciding] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [copiedKey, setCopiedKey] = useState<string | null>(null);
  const [showJump, setShowJump] = useState(false);
  // messagesExpanded 控制消息区放大阅读，方便查看长工具结果。
  const [messagesExpanded, setMessagesExpanded] = useState(false);
  const abortRef = useRef<AbortController | null>(null);
  const messageListRef = useRef<HTMLDivElement | null>(null);
  // stickToBottomRef 记录用户是否停留在底部，避免新内容把正在翻历史的用户拽回去。
  const stickToBottomRef = useRef(true);

  // 页面刷新或重新进入会话时，从最近 Run 列表恢复仍在执行的 Run，继续订阅 SSE。
  useEffect(() => {
    if (activeRun) {
      return;
    }
    const current = runs.data?.items.find((run) => !isTerminal(run.status));
    if (current) {
      setActiveRun(current);
    }
  }, [activeRun, runs.data]);

  const agent = useMemo(() => {
    const target = session.data?.agent_id;
    if (!target) {
      return null;
    }
    return agents.data?.items.find((item) => item.id === target) ?? null;
  }, [agents.data, session.data?.agent_id]);

  const canRun = Boolean(session.data?.agent_id && agent && agent.status === "published");
  const runActive = activeRun !== null && !isTerminal(activeRun.status);

  const refreshApprovals = useCallback(async (runID: string) => {
    try {
      const page = await requestJSON<{ items: ToolApproval[] }>(`/api/v1/runs/${runID}/approvals`);
      setApprovals(page.items);
    } catch {
      // 审批读取失败不阻断对话，错误由时间线提示。
    }
  }, []);

  // 消费当前 Run 的 SSE 事件流，断线后按游标重连。
  useEffect(() => {
    if (!activeRun) {
      return;
    }
    const runID = activeRun.id;
    const controller = new AbortController();
    abortRef.current = controller;
    let cancelled = false;
    let lastSeq = 0;
    setLiveMessages([]);
    setTimeline([]);
    setApprovals([]);
    setStreamError(null);
    setConnection("connecting");
    void refreshApprovals(runID);

    const pushTimeline = (entry: TimelineEntry) => {
      if (cancelled) {
        return;
      }
      setTimeline((previous) => (previous.some((item) => item.key === entry.key) ? previous : [...previous, entry].slice(-200)));
    };

    const handleEvent = (event: RunEvent) => {
      const payload = event.data ?? {};
      switch (event.event) {
        case "message.delta": {
          const delta = asString(payload.delta);
          if (!delta) {
            break;
          }
          const key = `delta:${asString(payload.worker) ?? ""}:${String(payload.step ?? "?")}`;
          setLiveMessages((previous) => {
            const existing = previous.find((item) => item.key === key);
            if (existing) {
              return previous.map((item) => (item.key === key ? { ...item, content: item.content + delta } : item));
            }
            return [...previous, { key, role: "assistant", content: delta }];
          });
          break;
        }
        case "message.created": {
          const role = asString(payload.role);
          const content = asString(payload.content) ?? "";
          if (role === "assistant" || role === "tool" || role === "system") {
            setLiveMessages((previous) => {
              // 完整消息落库后替换同一轮的流式草稿，避免气泡重复。
              const withoutDrafts = previous.filter((item) => !item.key.startsWith("delta:"));
              const key = `${event.seq}:${event.event}`;
              if (withoutDrafts.some((item) => item.key === key)) {
                return withoutDrafts;
              }
              return [...withoutDrafts, {
                key,
                role,
                content,
                toolCalls: payload.tool_calls,
                toolCallID: asString(payload.tool_call_id) ?? undefined,
                name: asString(payload.name) ?? undefined,
              }];
            });
          }
          break;
        }
        case "step.started":
          pushTimeline({ key: `${event.seq}:${event.event}`, label: `第 ${String(payload.step ?? "?")} 步开始`, tone: "info" });
          break;
        case "step.completed":
          pushTimeline({
            key: `${event.seq}:${event.event}`,
            label: `第 ${String(payload.step ?? "?")} 步完成`,
            detail: `输入 ${String(payload.input_tokens ?? 0)} / 输出 ${String(payload.output_tokens ?? 0)} Token`,
            tone: "success",
          });
          break;
        case "tool.started":
          pushTimeline({ key: `${event.seq}:${event.event}`, label: `调用工具 ${asString(payload.tool) ?? ""}`, detail: asString(payload.risk_level) ?? undefined, tone: "info" });
          break;
        case "tool.approval.required":
          pushTimeline({ key: `${event.seq}:${event.event}`, label: `等待人工审批：${asString(payload.tool) ?? ""}`, tone: "warning" });
          void refreshApprovals(runID);
          break;
        case "tool.approval.decided":
          pushTimeline({ key: `${event.seq}:${event.event}`, label: `审批已${payload.decision === "deny" ? "拒绝" : "通过"}`, tone: payload.decision === "deny" ? "danger" : "success" });
          void refreshApprovals(runID);
          break;
        case "tool.completed":
          pushTimeline({
            key: `${event.seq}:${event.event}`,
            label: `工具 ${asString(payload.tool) ?? ""} ${String(payload.status ?? "")}`,
            detail: asString(payload.error) ?? undefined,
            tone: payload.status === "completed" ? "success" : "warning",
          });
          break;
        case "context.assembled":
          pushTimeline({ key: `${event.seq}:${event.event}`, label: "上下文已组装", detail: payload.reasons ? JSON.stringify(payload.reasons) : undefined, tone: "info" });
          break;
        case "context.degraded":
          pushTimeline({ key: `${event.seq}:${event.event}`, label: "上下文已降级", detail: JSON.stringify(payload.components ?? payload), tone: "warning" });
          break;
        case "run.completed":
          pushTimeline({ key: `${event.seq}:${event.event}`, label: "处理完成", detail: `总 Token ${String(payload.total_tokens ?? "—")}`, tone: "success" });
          break;
        case "run.failed":
          pushTimeline({
            key: `${event.seq}:${event.event}`,
            label: "处理失败",
            detail: asString(payload.message) ?? asString(payload.reason) ?? undefined,
            tone: "danger",
          });
          break;
        case "plan.created":
          pushTimeline({ key: `${event.seq}:${event.event}`, label: "执行计划已生成", detail: `${String(payload.steps ?? "?")} 个步骤`, tone: "info" });
          break;
        case "plan.step.started":
          pushTimeline({
            key: `${event.seq}:${event.event}`,
            label: `计划步骤 ${String(payload.step_no ?? "?")} 开始：${asString(payload.title) ?? ""}`,
            detail: asString(payload.tool) ?? undefined,
            tone: "info",
          });
          break;
        case "plan.step.completed":
          pushTimeline({ key: `${event.seq}:${event.event}`, label: `计划步骤 ${String(payload.step_no ?? "?")} 完成`, tone: "success" });
          break;
        case "plan.step.failed":
          pushTimeline({
            key: `${event.seq}:${event.event}`,
            label: `计划步骤 ${String(payload.step_no ?? "?")} 失败`,
            detail: asString(payload.message) ?? asString(payload.code) ?? undefined,
            tone: "danger",
          });
          break;
        case "plan.step.waiting_approval":
          pushTimeline({
            key: `${event.seq}:${event.event}`,
            label: `计划步骤 ${String(payload.step_no ?? "?")} 等待审批`,
            detail: asString(payload.tool) ?? undefined,
            tone: "warning",
          });
          void refreshApprovals(runID);
          break;
        case "plan.completed":
          pushTimeline({ key: `${event.seq}:${event.event}`, label: "执行计划完成", detail: `总 Token ${String(payload.total_input_tokens ?? 0)} / ${String(payload.total_output_tokens ?? 0)}`, tone: "success" });
          break;
        case "plan.failed":
          pushTimeline({
            key: `${event.seq}:${event.event}`,
            label: "执行计划失败",
            detail: asString(payload.message) ?? asString(payload.code) ?? undefined,
            tone: "danger",
          });
          break;
        case "supervisor.tasks.created":
          pushTimeline({ key: `${event.seq}:${event.event}`, label: "Worker 委派已生成", detail: `${String(payload.workers ?? "?")} 个 Worker`, tone: "info" });
          break;
        case "supervisor.task.started":
          pushTimeline({
            key: `${event.seq}:${event.event}`,
            label: `Worker ${String(payload.index ?? "?")} 开始：${asString(payload.worker) ?? ""}`,
            tone: "info",
          });
          break;
        case "supervisor.task.completed":
          pushTimeline({ key: `${event.seq}:${event.event}`, label: `Worker ${String(payload.index ?? "?")} 完成`, tone: "success" });
          break;
        case "supervisor.task.failed":
          pushTimeline({
            key: `${event.seq}:${event.event}`,
            label: `Worker ${asString(payload.worker) ?? String(payload.index ?? "?")} ${statusLabel(asString(payload.status) ?? "failed")}`,
            detail: asString(payload.message) ?? asString(payload.code) ?? undefined,
            tone: asString(payload.status) === "cancelled" ? "warning" : "danger",
          });
          break;
        case "supervisor.completed":
          pushTimeline({
            key: `${event.seq}:${event.event}`,
            label: payload.partial === true ? "Supervisor 部分完成" : "Supervisor 全部完成",
            detail: `完成 ${String(payload.completed ?? 0)} / 失败 ${String(payload.failed ?? 0)} / 取消 ${String(payload.cancelled ?? 0)}`,
            tone: payload.partial === true ? "warning" : "success",
          });
          break;
        case "supervisor.failed":
          pushTimeline({ key: `${event.seq}:${event.event}`, label: "Supervisor 全部失败", detail: `失败 ${String(payload.failed ?? 0)}`, tone: "danger" });
          break;
        case "supervisor.budget_exceeded":
          pushTimeline({ key: `${event.seq}:${event.event}`, label: "Supervisor 共享预算耗尽", tone: "danger" });
          break;
        default:
          break;
      }
    };

    const consume = async () => {
      for (let attempt = 0; attempt < 8 && !cancelled; attempt += 1) {
        try {
          await streamRunEvents(runID, {
            signal: controller.signal,
            lastEventId: lastSeq,
            onOpen: () => setConnection("streaming"),
            onEvent: (event) => {
              if (event.seq > 0) {
                if (event.seq <= lastSeq) {
                  return;
                }
                lastSeq = event.seq;
              }
              handleEvent(event);
            },
          });
        } catch (streamFailure) {
          if (cancelled || controller.signal.aborted) {
            return;
          }
          setStreamError(describeError(streamFailure));
        }
        if (cancelled) {
          return;
        }
        try {
          const current = await requestJSON<Run>(`/api/v1/runs/${runID}`);
          setActiveRun(current);
          if (isTerminal(current.status)) {
            setConnection("closed");
            void refreshApprovals(runID);
            // 无论成功、失败还是取消都重新拉取消息并清空临时流，避免与已落库消息重复。
            messagesReload();
            setLiveMessages([]);
            setRunsVersion((value) => value + 1);
            return;
          }
        } catch (statusFailure) {
          setStreamError(describeError(statusFailure));
        }
        setConnection("reconnecting");
        await sleep(Math.min(1000 * (attempt + 1), 4000), controller.signal);
      }
      setConnection("closed");
    };

    void consume();
    return () => {
      cancelled = true;
      controller.abort();
      abortRef.current = null;
    };
  }, [activeRun?.id, messagesReload, refreshApprovals]);

  // uploadFiles 逐个上传附件，超限或失败的文件在输入区内提示，不阻塞其它文件。
  const uploadFiles = async (files: File[]) => {
    const room = MAX_ATTACHMENTS_PER_MESSAGE - pendingAttachments.length;
    if (room <= 0) {
      setSendError(`每条消息最多 ${MAX_ATTACHMENTS_PER_MESSAGE} 个附件。`);
      return;
    }
    const accepted = files.slice(0, room);
    if (files.length > room) {
      setSendError(`超出数量上限，只会上传前 ${room} 个文件。`);
    }
    for (const file of accepted) {
      if (file.size > MAX_ATTACHMENT_BYTES) {
        setSendError(`${file.name} 超过 20MB，未上传。`);
        continue;
      }
      if (file.size === 0) {
        setSendError(`${file.name} 是空文件，未上传。`);
        continue;
      }
      const key = crypto.randomUUID();
      setPendingAttachments((current) => [...current, { key, name: file.name, size: file.size, status: "uploading" }]);
      try {
        const attachment = await uploadAttachment<Attachment>(`/api/v1/sessions/${sessionId}/attachments`, file);
        if (removedAttachmentKeys.current.has(key)) {
          removedAttachmentKeys.current.delete(key);
          // 上传完成前用户已移除该文件：立即删除服务端对象，避免占用未绑定附件配额。
          try {
            await requestJSON<void>(`/api/v1/attachments/${encodeURIComponent(attachment.id)}`, { method: "DELETE" });
          } catch {
            // 删除失败由服务端配额回收兜底，不阻塞其它附件。
          }
        } else {
          setPendingAttachments((current) => current.map((item) => (item.key === key ? { ...item, status: "ready", id: attachment.id } : item)));
        }
      } catch (failure) {
        const message = describeError(failure);
        setPendingAttachments((current) => current.map((item) => (item.key === key ? { ...item, status: "failed", error: message } : item)));
      }
    }
  };

  // removePendingAttachment 从输入区移除附件，并回收已上传但未发送的服务端文件。
  const removePendingAttachment = async (key: string) => {
    const target = pendingAttachments.find((item) => item.key === key);
    if (target?.status === "uploading") {
      removedAttachmentKeys.current.add(key);
    }
    setPendingAttachments((current) => current.filter((item) => item.key !== key));
    if (target?.id) {
      try {
        await requestJSON<void>(`/api/v1/attachments/${encodeURIComponent(target.id)}`, { method: "DELETE" });
      } catch (failure) {
        // 附件已从输入区移除；服务端删除失败不阻塞继续编辑，错误由页面提示。
        setSendError(describeError(failure));
      }
    }
  };

  // downloadMessageAttachment 鉴权下载消息附件，失败时在页面内提示。
  const downloadMessageAttachment = async (attachment: Attachment) => {
    setActionError(null);
    try {
      await downloadAttachment(attachment.download_url, attachment.file_name);
    } catch (failure) {
      setActionError(describeError(failure));
    }
  };

  const send = async (contentOverride?: string) => {
    const content = (contentOverride ?? draft).trim();
    const readyAttachments = pendingAttachments.filter((item) => item.status === "ready" && item.id);
    if (pendingAttachments.some((item) => item.status === "uploading")) {
      setSendError("附件仍在上传，请稍候再发送。");
      return;
    }
    if ((!content && readyAttachments.length === 0) || sending || runActive) {
      return;
    }
    if (!canRun) {
      setSendError("这个对话没有绑定可用助手。请先在助手页发布一个助手，再新建对话。");
      return;
    }
    setSending(true);
    setSendError(null);
    setActionError(null);
    // 发送后强制跟随新消息，即使之前正在向上翻阅历史。
    stickToBottomRef.current = true;
    setShowJump(false);
    try {
      const message = await mutate<Message>(`/api/v1/sessions/${sessionId}/messages`, "POST", {
        // 仅附件时补一句正文，保证消息可读且执行器有明确任务描述。
        content: content || "我上传了附件，请结合附件内容回答。",
        attachment_ids: readyAttachments.map((item) => item.id),
      }, crypto.randomUUID());
      // 重试上一轮时保留输入框内容，避免用户正在编辑的新内容被清空。
      if (contentOverride === undefined) {
        setDraft("");
      }
      setPendingAttachments([]);
      messagesReload();
      const run = await mutate<Run>(`/api/v1/sessions/${sessionId}/runs`, "POST", {
        agent_id: agent?.id ?? null,
        agent_version: agent?.version ?? null,
        mode: agent?.mode ?? "react",
        trigger_message_id: message.id,
      }, crypto.randomUUID());
      setActiveRun(run);
    } catch (failure) {
      setSendError(describeError(failure));
    } finally {
      setSending(false);
    }
  };

  const cancel = async () => {
    if (!activeRun) {
      return;
    }
    setActionError(null);
    try {
      const updated = await mutate<Run>(`/api/v1/runs/${activeRun.id}/cancel`, "POST", undefined, crypto.randomUUID());
      setActiveRun(updated);
      abortRef.current?.abort();
      messagesReload();
    } catch (failure) {
      setActionError(describeError(failure));
    }
  };

  const decide = async (approval: ToolApproval, decision: "approve" | "deny") => {
    if (!activeRun) {
      return;
    }
    setDeciding(approval.id);
    setActionError(null);
    try {
      await mutate<ToolApproval>(`/api/v1/runs/${activeRun.id}/approvals/${approval.id}`, "POST", { decision }, crypto.randomUUID());
      await refreshApprovals(activeRun.id);
      const current = await requestJSON<Run>(`/api/v1/runs/${activeRun.id}`);
      setActiveRun(current);
      toast.show(decision === "approve" ? "已批准，助手继续执行" : "已拒绝该工具调用", decision === "approve" ? "success" : "info");
    } catch (failure) {
      setActionError(describeError(failure));
    } finally {
      setDeciding(null);
    }
  };

  // loadOlderMessages 按当前页游标继续向前加载历史消息。
  const loadOlderMessages = async () => {
    const cursor = olderCursor ?? messages.data?.next_cursor ?? null;
    if (!cursor || olderLoading) {
      return;
    }
    setOlderLoading(true);
    setActionError(null);
    try {
      const page = await requestJSON<Page<Message>>(
        `/api/v1/sessions/${sessionId}/messages?limit=100&cursor=${encodeURIComponent(cursor)}`,
      );
      const ascending = [...page.items].reverse();
      setOlderMessages((current) => [...ascending, ...current]);
      setOlderCursor(page.next_cursor ?? null);
      setOlderEnded(!page.next_cursor);
    } catch (failure) {
      setActionError(describeError(failure));
    } finally {
      setOlderLoading(false);
    }
  };

  const pendingApprovals = approvals.filter((approval) => approval.status === "pending");
  const persistedMessages = [...olderMessages, ...(messages.data?.items ?? []).slice().reverse()];

  // 最近一条用户消息用于失败后的“重试本轮”和“编辑后重发”。
  const lastUserContent = useMemo(() => {
    for (let index = persistedMessages.length - 1; index >= 0; index -= 1) {
      if (persistedMessages[index].role === "user") {
        return persistedMessages[index].content;
      }
    }
    return null;
  }, [persistedMessages]);

  const latestRun = runs.data?.items[0] ?? null;
  // 当前没有运行中的任务时，才把失败结果展示在主对话区；优先使用本轮状态，避免历史列表短暂滞后。
  const failedRun = runActive
    ? null
    : activeRun
      ? (activeRun.status === "failed" ? activeRun : null)
      : (latestRun?.status === "failed" ? latestRun : null);

  // 新内容到达时只在用户停留在底部才自动滚动，向上翻阅历史时不打断阅读。
  useEffect(() => {
    const element = messageListRef.current;
    if (!element) {
      return;
    }
    if (stickToBottomRef.current) {
      element.scrollTop = element.scrollHeight;
      setShowJump(false);
      return;
    }
    if (persistedMessages.length + liveMessages.length > 0) {
      setShowJump(true);
    }
  }, [persistedMessages.length, liveMessages.length, runActive, messagesExpanded]);

  // 放大阅读时锁定页面滚动，并支持 Esc 恢复。
  useEffect(() => {
    if (!messagesExpanded) {
      return;
    }
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        setMessagesExpanded(false);
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => {
      document.body.style.overflow = previousOverflow;
      window.removeEventListener("keydown", onKeyDown);
    };
  }, [messagesExpanded]);

  // scrollToLatest 主动回到底部，并恢复自动跟随。
  const scrollToLatest = () => {
    const element = messageListRef.current;
    if (!element) {
      return;
    }
    element.scrollTop = element.scrollHeight;
    stickToBottomRef.current = true;
    setShowJump(false);
  };

  // onMessageScroll 根据距底部距离切换自动跟随状态。
  const onMessageScroll = () => {
    const element = messageListRef.current;
    if (!element) {
      return;
    }
    const distance = element.scrollHeight - element.scrollTop - element.clientHeight;
    const nearBottom = distance <= 48;
    stickToBottomRef.current = nearBottom;
    setShowJump(!nearBottom && persistedMessages.length + liveMessages.length > 0);
  };

  const copyMessage = async (key: string, content: string) => {
    try {
      await navigator.clipboard.writeText(content);
      setCopiedKey(key);
      window.setTimeout(() => setCopiedKey((current) => (current === key ? null : current)), 1500);
    } catch {
      setActionError("复制失败，请手动选择文本。");
    }
  };

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        eyebrow="对话"
        title={session.data?.title ?? "加载对话…"}
        description={session.data ? `${statusLabel(session.data.status)} · 来自${sourceLabel(session.data.source)} · 最近消息 ${formatRelative(session.data.last_message_at)}` : undefined}
        actions={
          <Link to="/sessions" className="inline-flex h-10 items-center rounded-[3px] border border-line-strong bg-paper-raised px-4 text-sm text-ink hover:border-accent">
            返回对话列表
          </Link>
        }
      />
      {session.error ? <ErrorBanner message={session.error} onRetry={session.reload} /> : null}
      {sendError ? <ErrorBanner message={sendError} /> : null}
      {actionError ? <ErrorBanner message={actionError} /> : null}
      {streamError ? <ErrorBanner message={`事件流中断：${streamError}`} /> : null}

      <div className="grid gap-6 xl:grid-cols-[minmax(0,1fr)_380px] xl:items-start">
      <Card
        title={agent?.name ?? "对话"}
        description={canRun ? `${modeLabel(agent?.mode)} · 消息会自动保存，处理过程可随时回看` : "这个对话没有绑定可用助手，只能查看历史消息。"}
        actions={
          <>
            {runActive ? <Badge tone="warning">正在处理</Badge> : null}
            <Button
              size="sm"
              variant="secondary"
              aria-pressed={messagesExpanded}
              onClick={() => setMessagesExpanded((current) => !current)}
            >
              {messagesExpanded ? "恢复窗口" : "放大阅读"}
            </Button>
          </>
        }
      >
        {(() => {
          // 放大阅读必须用 Portal 挂到 body：页面根节点带入场动画的 transform，
          // 会让 fixed 相对该容器定位，只有跳出这一层才能真正铺满视口。
          const log = (
            <div
              ref={messageListRef}
              className={cn(
                "flex flex-col gap-4 overflow-y-auto px-4 py-5",
                messagesExpanded
                  ? "min-h-0 flex-1 rounded-b-[3px] bg-paper/60"
                  : "max-h-[calc(100vh-24rem)] min-h-[340px] rounded-[3px] border border-line bg-paper/60",
              )}
              role="log"
              aria-live="polite"
              onScroll={onMessageScroll}
            >
          {messages.loading ? (
            <LoadingState label="加载消息" />
          ) : persistedMessages.length === 0 && liveMessages.length === 0 ? (
            <div className="flex flex-1 flex-col items-center justify-center gap-5 py-8 text-center">
              <span aria-hidden className="flex size-12 items-center justify-center rounded-full border border-line-strong font-display text-xl text-accent">
                助
              </span>
              <div>
                <p className="text-lg text-ink">向{agent?.name ?? "助手"}描述你想完成什么</p>
                <p className="mt-1 text-xs leading-5 text-muted">可以是一个目标、一段材料，或一个需要整理的问题。</p>
              </div>
              {canRun ? (
                <div className="grid w-full max-w-xl gap-2 sm:grid-cols-3">
                  {["帮我梳理这段需求", "把材料拆成待办", "解释一下这个报错"].map((prompt) => (
                    <button
                      key={prompt}
                      type="button"
                      onClick={() => {
                        setDraft(prompt);
                        document.getElementById("chat-composer")?.focus();
                      }}
                      className="rounded-[3px] border border-line bg-paper-raised px-3 py-2.5 text-left text-xs leading-5 text-ink-soft transition hover:border-accent hover:text-accent"
                    >
                      {prompt}
                    </button>
                  ))}
                </div>
              ) : (
                <p className="text-xs leading-5 text-muted">请先在助手页发布一个助手，再新建绑定该助手的对话。</p>
              )}
            </div>
          ) : (
            <>
              {!olderEnded && (olderCursor || messages.data?.next_cursor) ? (
                <div className="flex justify-center py-2">
                  <Button size="sm" variant="secondary" loading={olderLoading} onClick={() => void loadOlderMessages()}>
                    加载更早消息
                  </Button>
                </div>
              ) : null}
              {persistedMessages.map((message) => (
                <MessageBubble
                  key={message.id}
                  messageKey={message.id}
                  role={message.role}
                  content={message.content}
                  toolCalls={message.tool_calls}
                  attachments={message.attachments}
                  onDownloadAttachment={downloadMessageAttachment}
                  createdAt={message.created_at}
                  copied={copiedKey === message.id}
                  onCopy={copyMessage}
                />
              ))}
              {liveMessages.map((message) => (
                <MessageBubble
                  key={message.key}
                  messageKey={message.key}
                  role={message.role}
                  content={message.content}
                  toolCalls={message.toolCalls}
                  name={message.name}
                  live
                  copied={copiedKey === message.key}
                  onCopy={copyMessage}
                />
              ))}
            </>
          )}
          {failedRun ? (
            <div role="alert" className="rounded-[3px] border border-rust bg-rust-soft/60 px-4 py-3">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <p className="text-sm font-medium text-rust">本轮处理失败</p>
                <Link to={`/runs/${failedRun.id}`} className="text-xs text-accent underline underline-offset-2">
                  查看完整过程
                </Link>
              </div>
              <p className="mt-1 break-all text-xs leading-5 text-rust">
                {failedRun.error_code ? `${failedRun.error_code}：` : ""}{failedRun.error_message || "未返回具体原因，可打开运行详情查看执行步骤。"}
              </p>
              {lastUserContent ? (
                <div className="mt-3 flex flex-wrap gap-2">
                  <Button size="sm" onClick={() => void send(lastUserContent)} disabled={!canRun || runActive || sending}>
                    重试本轮
                  </Button>
                  <Button
                    size="sm"
                    variant="secondary"
                    onClick={() => {
                      setDraft(lastUserContent);
                      document.getElementById("chat-composer")?.focus();
                    }}
                  >
                    编辑后重发
                  </Button>
                </div>
              ) : null}
            </div>
          ) : null}
          {runActive ? (
            <p className="flex items-center gap-2 text-xs text-muted" role="status">
              <span className="flex gap-1" aria-hidden>
                <span className="size-1.5 animate-bounce rounded-full bg-accent [animation-delay:-0.2s]" />
                <span className="size-1.5 animate-bounce rounded-full bg-accent [animation-delay:-0.1s]" />
                <span className="size-1.5 animate-bounce rounded-full bg-accent" />
              </span>
              助手正在处理第 {activeRun?.current_step_seq || 1} 步…
            </p>
          ) : null}
          {showJump ? (
            <button
              type="button"
              onClick={scrollToLatest}
              className="sticky bottom-0 z-10 mx-auto rounded-[3px] border border-line-strong bg-paper-raised px-3 py-1.5 text-xs text-ink shadow-[0_6px_20px_rgb(31_27_21/0.12)] transition hover:border-accent hover:text-accent"
            >
              有新内容，回到底部
            </button>
          ) : null}
            </div>
          );
          if (!messagesExpanded) {
            return log;
          }
          return createPortal(
            <>
              <button
                type="button"
                aria-label="退出放大阅读"
                tabIndex={-1}
                onClick={() => setMessagesExpanded(false)}
                className="fixed inset-0 z-40 cursor-default bg-ink/25"
              />
              <div className="fixed inset-4 z-50 flex flex-col rounded-[3px] border border-line bg-paper-raised shadow-[0_24px_80px_rgb(31_27_21/0.28)]">
                <div className="flex flex-wrap items-center justify-between gap-2 border-b border-line px-4 py-2">
                  <p className="text-xs text-muted">放大阅读 · 按 Esc 或点击「恢复窗口」返回</p>
                  <Button size="sm" variant="secondary" onClick={() => setMessagesExpanded(false)}>恢复窗口</Button>
                </div>
                {log}
              </div>
            </>,
            document.body,
          );
        })()}

        {pendingApprovals.length > 0 ? (
          <div className="mt-4 rounded-[3px] border border-ochre bg-ochre-soft/60 p-3">
            <div className="flex flex-wrap items-baseline gap-2">
              <p className="text-sm font-medium text-ochre">需要你确认（{pendingApprovals.length}）</p>
              <p className="text-xs text-muted">高风险工具在继续执行前需要你的批准。</p>
            </div>
            <div className="mt-3 flex flex-col gap-2">
              {pendingApprovals.map((approval) => {
                const fields = approvalFields(approval.arguments);
                return (
                  <div key={approval.id} className="rounded-[3px] border border-ochre/60 bg-paper-raised p-3 text-xs">
                    <div className="flex flex-wrap items-center justify-between gap-2">
                      <p className="font-medium text-ink">{approval.tool_name}</p>
                      <Badge tone={approval.risk_level === "high" ? "danger" : "warning"}>{approval.risk_level === "high" ? "高风险" : approval.risk_level === "medium" ? "需留意" : "低风险"}</Badge>
                    </div>
                    {fields.length > 0 ? (
                      <div className="mt-2 max-h-40 overflow-auto rounded bg-paper-deep/40 p-2">
                        <div className="flex flex-col gap-1">
                          {fields.map(([key, value]) => (
                            <div key={key} className="grid grid-cols-[110px_1fr] gap-2 text-[11px]">
                              <span className="font-mono text-muted">{key}</span>
                              <span className="break-all font-mono text-ink-soft">{value}</span>
                            </div>
                          ))}
                        </div>
                      </div>
                    ) : (
                      <p className="mt-2 text-[11px] text-muted">这个工具没有参数。</p>
                    )}
                    <div className="mt-2 flex flex-wrap gap-2">
                      <Button size="sm" loading={deciding === approval.id} onClick={() => void decide(approval, "approve")}>批准并继续</Button>
                      <Button size="sm" variant="danger" loading={deciding === approval.id} onClick={() => void decide(approval, "deny")}>拒绝</Button>
                      {approval.arguments ? (
                        <Button
                          size="sm"
                          variant="ghost"
                          onClick={() => void copyMessage(`approval-${approval.id}`, JSON.stringify(approval.arguments, null, 2))}
                        >
                          {copiedKey === `approval-${approval.id}` ? "已复制" : "复制参数"}
                        </Button>
                      ) : null}
                    </div>
                  </div>
                );
              })}
            </div>
          </div>
        ) : null}

        <div className="sticky bottom-3 z-10 mt-4 rounded-[3px] border border-line-strong bg-paper-raised/95 p-3 shadow-[0_-6px_20px_rgb(31_27_21/0.06)] backdrop-blur focus-within:border-accent">
          <input
            ref={fileInputRef}
            type="file"
            multiple
            className="hidden"
            aria-label="选择附件"
            onChange={(event) => {
              const files = Array.from(event.target.files ?? []);
              event.target.value = "";
              if (files.length > 0) {
                void uploadFiles(files);
              }
            }}
          />
          {pendingAttachments.length > 0 ? (
            <div className="mb-2 flex flex-col gap-1.5 rounded-[3px] border border-line bg-paper-deep/40 p-2">
              {pendingAttachments.map((item) => (
                <div key={item.key} className={cn("flex items-center gap-2 text-xs", item.status === "failed" ? "text-rust" : "text-ink-soft")}>
                  <span className="shrink-0" aria-hidden>📎</span>
                  <span className="min-w-0 flex-1 truncate">{item.name}</span>
                  <span className="shrink-0 text-[11px] text-muted">{formatFileSize(item.size)}</span>
                  <span className="shrink-0 text-[11px]">
                    {item.status === "uploading" ? "上传中…" : item.status === "failed" ? item.error ?? "上传失败" : "待发送"}
                  </span>
                  <button
                    type="button"
                    onClick={() => void removePendingAttachment(item.key)}
                    className="shrink-0 rounded-[2px] px-1 text-muted transition hover:text-rust"
                    aria-label={`移除附件 ${item.name}`}
                  >
                    ×
                  </button>
                </div>
              ))}
            </div>
          ) : null}
          <Textarea
            id="chat-composer"
            value={draft}
            onChange={(event) => setDraft(event.target.value)}
            placeholder={canRun ? "说说你想完成什么…" : "这个对话没有绑定可用助手，暂时不能发送"}
            disabled={!canRun || runActive || sending}
            className="min-h-20 resize-none border-0 bg-transparent p-0 focus:ring-0"
            aria-label="消息内容"
            onKeyDown={(event) => {
              // 输入法组合阶段按 Enter 只用于确认候选词，不触发发送。
              if (event.nativeEvent.isComposing) {
                return;
              }
              if (event.key === "Enter" && !event.shiftKey) {
                event.preventDefault();
                void send();
              }
            }}
          />
          <div className="mt-2 flex flex-wrap items-center justify-between gap-2">
            <p className="text-[11px] text-muted">
              {runActive ? "助手正在处理，发送暂时不可用。" : "Enter 发送 · Shift + Enter 换行 · 单个文件 ≤20MB，最多 5 个，文本 / PDF / Word / Excel 内容会自动提供给助手"}
            </p>
            <div className="flex gap-2">
              <Button
                size="sm"
                variant="ghost"
                onClick={() => fileInputRef.current?.click()}
                disabled={!canRun || runActive || sending || pendingAttachments.length >= MAX_ATTACHMENTS_PER_MESSAGE}
              >
                添加附件
              </Button>
              {runActive ? (
                <Button size="sm" variant="secondary" onClick={() => void cancel()}>停止本轮</Button>
              ) : null}
              <Button
                size="sm"
                onClick={() => void send()}
                loading={sending}
                disabled={!canRun || runActive || (draft.trim() === "" && pendingAttachments.filter((item) => item.status === "ready").length === 0) || pendingAttachments.some((item) => item.status === "uploading")}
              >
                发送
              </Button>
            </div>
          </div>
        </div>
      </Card>

        <div className="flex flex-col gap-4 xl:sticky xl:top-20">
        <Card title="处理过程" description="默认收起。摘要会显示当前进度，展开后才能查看每一步。">
          {activeRun ? (
            <details className="group rounded-[3px] border border-line bg-paper-raised">
              <summary className="flex cursor-pointer list-none flex-wrap items-center gap-2 rounded-[3px] px-3 py-2 text-xs transition hover:bg-paper-deep [&::-webkit-details-marker]:hidden">
                <Badge tone={statusTone(activeRun.status)}>{statusLabel(activeRun.status)}</Badge>
                {runActive ? (
                  <span className="flex items-center gap-1.5 text-accent">
                    <span aria-hidden className="size-1.5 animate-pulse rounded-full bg-accent" />
                    第 {activeRun.current_step_seq || 1} 步
                  </span>
                ) : (
                  <span className="text-ink">共 {activeRun.current_step_seq || 0} 步</span>
                )}
                <span className="text-muted">{timeline.length} 条过程记录</span>
                <span className="ml-auto text-accent group-open:hidden">展开</span>
                <span className="ml-auto hidden text-accent group-open:inline">收起</span>
              </summary>
              <div className="flex flex-col gap-3 border-t border-line p-3">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="text-xs text-muted">{{ idle: "未开始", connecting: "连接中", streaming: "实时接收", reconnecting: "重连中", closed: "已结束" }[connection]}</span>
                  <span className="text-xs text-muted">{modeLabel(activeRun.mode)} · 尝试 {activeRun.attempt_no} 次</span>
                </div>
                {activeRun.error_message ? <p className="rounded-[3px] border border-rust bg-rust-soft px-2 py-1 text-xs text-rust">{activeRun.error_code}: {activeRun.error_message}</p> : null}
                {timeline.length > 0 ? (
                  <ol className="flex max-h-72 flex-col gap-2 overflow-y-auto" aria-label="处理时间线">
                    {timeline.map((entry) => (
                      <li key={entry.key} className="flex items-start gap-2 text-xs">
                        <span className={cn("mt-1 size-1.5 shrink-0 rounded-full", entry.tone === "success" ? "bg-accent" : entry.tone === "danger" ? "bg-rust" : entry.tone === "warning" ? "bg-ochre" : "bg-jade")} />
                        <div>
                          <p className="text-ink">
                            <Link to={`/runs/${activeRun.id}`} className="underline-offset-2 hover:text-accent hover:underline">
                              {entry.label}
                            </Link>
                          </p>
                          {entry.detail ? <p className="break-all text-[11px] text-muted">{entry.detail}</p> : null}
                        </div>
                      </li>
                    ))}
                  </ol>
                ) : (
                  <p className="text-xs text-muted">等待执行事件…</p>
                )}
              </div>
            </details>
          ) : (
            <p className="text-xs leading-5 text-muted">发送消息后，这里会实时显示思考步骤、工具调用和错误原因；SSE 断线会自动重连。</p>
          )}
        </Card>

        <Card title="历史处理记录" description="默认收起。展开后点“查看过程”可以看完整输入输出。">
          {runs.loading ? (
            <LoadingState />
          ) : runs.data && runs.data.items.length > 0 ? (
            <details className="group rounded-[3px] border border-line bg-paper-raised">
              <summary className="flex cursor-pointer list-none flex-wrap items-center gap-2 rounded-[3px] px-3 py-2 text-xs transition hover:bg-paper-deep [&::-webkit-details-marker]:hidden">
                <span className="text-ink">最近 {runs.data.items.length} 次处理</span>
                <span className="text-muted">展开查看</span>
                <span className="ml-auto text-accent group-open:hidden">展开</span>
                <span className="ml-auto hidden text-accent group-open:inline">收起</span>
              </summary>
              <div className="flex flex-col divide-y divide-line border-t border-line px-3">
                {runs.data.items.map((run) => (
                  <div key={run.id} className="flex flex-wrap items-center justify-between gap-2 py-2 text-sm">
                    <div className="flex items-center gap-2">
                      <Badge tone={statusTone(run.status)}>{statusLabel(run.status)}</Badge>
                      <span className="text-xs text-muted">{modeLabel(run.mode)} · {formatDateTime(run.created_at)}</span>
                    </div>
                    <Link to={`/runs/${run.id}`} className="text-xs text-accent underline underline-offset-2">查看过程</Link>
                  </div>
                ))}
              </div>
            </details>
          ) : (
            <EmptyState title="还没有处理记录" description="发送第一条消息后，这里会显示完整处理过程。" />
          )}
        </Card>
        </div>
      </div>
    </div>
  );
}

// MessageBubble 渲染单条消息；助手内容使用安全 Markdown，并支持一键复制。
function MessageBubble({
  messageKey,
  role,
  content,
  toolCalls,
  name,
  createdAt,
  attachments,
  onDownloadAttachment,
  live = false,
  copied,
  onCopy,
}: {
  messageKey: string;
  role: string;
  content: string;
  toolCalls?: unknown;
  name?: string;
  createdAt?: string;
  attachments?: Attachment[];
  onDownloadAttachment?: (attachment: Attachment) => void;
  live?: boolean;
  copied: boolean;
  onCopy: (key: string, content: string) => Promise<void>;
}) {
  const toolNames = toolCallNames(toolCalls);
  const hasToolCalls = Array.isArray(toolCalls) && toolCalls.length > 0;
  if (role === "system") {
    return <div className="mx-auto max-w-[90%] rounded-[3px] border border-line bg-paper-deep/40 px-3 py-2 text-center text-[11px] leading-5 text-muted">{content}</div>;
  }
  if (role === "user") {
    return (
      <div className="flex justify-end">
        <div className="max-w-[85%]">
          <div className="whitespace-pre-wrap break-words rounded-[4px] bg-jade px-4 py-3 text-sm leading-6 text-paper-raised">{content}</div>
          {attachments && attachments.length > 0 ? (
            <div className="mt-2 flex flex-col gap-1">
              {attachments.map((attachment) => (
                <button
                  key={attachment.id}
                  type="button"
                  onClick={() => onDownloadAttachment?.(attachment)}
                  className="flex items-center gap-2 rounded-[3px] border border-line bg-paper-raised px-2 py-1 text-left text-[11px] text-ink-soft transition hover:border-accent hover:text-accent"
                  aria-label={`下载附件 ${attachment.file_name}`}
                >
                  <span aria-hidden>📎</span>
                  <span className="min-w-0 flex-1 truncate">{attachment.file_name}</span>
                  <span className="shrink-0 text-muted">{formatFileSize(attachment.size_bytes)}</span>
                  <span className="shrink-0 text-muted">
                    {attachment.text_extracted ? (attachment.text_truncated ? "已读取（截断）" : "已读取内容") : "仅保存原件"}
                  </span>
                </button>
              ))}
            </div>
          ) : null}
          {createdAt ? <p className="mt-1 text-right text-[11px] text-muted">{formatDateTime(createdAt)}</p> : null}
        </div>
      </div>
    );
  }
  if (role === "tool") {
    return (
      <div className="ml-11 max-w-[85%]">
        <details className="rounded-[3px] border border-line bg-paper-deep/50 px-3 py-2 text-xs text-muted">
          <summary className="cursor-pointer text-muted">工具结果{name ? ` · ${name}` : ""}{toolCalls ? "（含调用记录）" : ""}</summary>
          <pre className="mt-2 max-h-48 overflow-auto whitespace-pre-wrap break-all font-mono text-[11px]">{content}</pre>
        </details>
      </div>
    );
  }
  // 带工具调用的助手消息属于中间思考过程，默认折叠，只保留最终回答展开。
  if (hasToolCalls) {
    return (
      <div className="flex gap-3">
        <span aria-hidden className="mt-1 flex size-8 shrink-0 items-center justify-center rounded-full border border-line-strong bg-paper-raised font-display text-sm text-accent">
          助
        </span>
        <details className="group min-w-0 max-w-[85%] flex-1 rounded-[4px] border border-line bg-paper-raised">
          <summary className="flex cursor-pointer list-none flex-wrap items-center gap-2 rounded-[4px] px-4 py-3 text-[11px] text-muted [&::-webkit-details-marker]:hidden">
            <span className="text-sm font-medium text-ink-soft">助手思考</span>
            {live ? (
              <span className="flex items-center gap-1 text-accent">
                <span aria-hidden className="size-1.5 animate-pulse rounded-full bg-accent" />
                实时接收
              </span>
            ) : null}
            <span>调用 {toolNames.length} 个工具</span>
            {createdAt ? <span>{formatDateTime(createdAt)}</span> : null}
            <span className="ml-auto text-accent group-open:hidden">展开</span>
            <span className="ml-auto hidden text-accent group-open:inline">收起</span>
          </summary>
          <div className="border-t border-line px-4 py-3 text-sm leading-6">
            {content ? (
              <SafeMarkdown content={content} />
            ) : (
              <p className="text-xs text-muted">正在调用工具，等待结果…</p>
            )}
            {toolNames.length > 0 ? (
              <p className="mt-2 flex flex-wrap items-center gap-1 text-[11px] text-muted">
                调用工具：
                {toolNames.map((toolName, index) => (
                  <span key={`${toolName}-${index}`} className="rounded-[2px] border border-line bg-paper-deep/50 px-1.5 py-0.5 font-mono">
                    {toolName}
                  </span>
                ))}
              </p>
            ) : null}
            {hasToolCalls ? (
              <details className="mt-2 text-[11px] text-muted">
                <summary className="cursor-pointer">工具调用记录（{toolCalls.length}）</summary>
                <pre className="mt-1 overflow-auto whitespace-pre-wrap break-all rounded bg-paper-deep/40 p-2 font-mono">{JSON.stringify(toolCalls, null, 2)}</pre>
              </details>
            ) : null}
          </div>
        </details>
      </div>
    );
  }
  return (
    <div className="flex gap-3">
      <span aria-hidden className="mt-1 flex size-8 shrink-0 items-center justify-center rounded-full border border-line-strong bg-paper-raised font-display text-sm text-accent">
        助
      </span>
      <div className="min-w-0 max-w-[85%] flex-1">
        <div className="mb-1 flex flex-wrap items-center gap-2 text-[11px] text-muted">
          <span className="font-medium text-ink-soft">助手</span>
          {live ? (
            <span className="flex items-center gap-1 text-accent">
              <span aria-hidden className="size-1.5 animate-pulse rounded-full bg-accent" />
              实时接收
            </span>
          ) : null}
          {createdAt ? <span>{formatDateTime(createdAt)}</span> : null}
          {content ? (
            <button
              type="button"
              onClick={() => void onCopy(messageKey, content)}
              className="ml-auto rounded-[2px] border border-line px-1.5 py-0.5 text-[11px] text-muted transition hover:border-accent hover:text-accent"
            >
              {copied ? "已复制" : "复制"}
            </button>
          ) : null}
        </div>
        <div className="rounded-[4px] border border-line bg-paper-raised px-4 py-3 text-sm leading-6">
          {content ? (
            <SafeMarkdown content={content} />
          ) : (
            <p className="text-xs text-muted">{toolNames.length > 0 ? "正在调用工具，等待结果…" : "这一步没有文本输出。"}</p>
          )}
          {toolNames.length > 0 ? (
            <p className="mt-2 flex flex-wrap items-center gap-1 text-[11px] text-muted">
              调用工具：
              {toolNames.map((toolName, index) => (
                <span key={`${toolName}-${index}`} className="rounded-[2px] border border-line bg-paper-deep/50 px-1.5 py-0.5 font-mono">
                  {toolName}
                </span>
              ))}
            </p>
          ) : null}
          {Array.isArray(toolCalls) && toolCalls.length > 0 ? (
            <details className="mt-2 text-[11px] text-muted">
              <summary className="cursor-pointer">工具调用记录（{toolCalls.length}）</summary>
              <pre className="mt-1 overflow-auto whitespace-pre-wrap break-all rounded bg-paper-deep/40 p-2 font-mono">{JSON.stringify(toolCalls, null, 2)}</pre>
            </details>
          ) : null}
        </div>
      </div>
    </div>
  );
}
