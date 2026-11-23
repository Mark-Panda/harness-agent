// 会话对话：发送消息、创建 Run、消费 SSE 增量、审批工具并展示执行时间线。
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { Icon } from "../components/Icon";
import { MessageBubble, ProcessRecord, formatFileSize, toolCallNames } from "../components/ChatMessage";
import { useToast } from "../components/Toast";
import { ActionLink, Badge, Button, ConfirmDialog, ErrorBanner, LoadingState, cn } from "../components/ui";
import { downloadAttachment, mutate, requestJSON, streamRunEvents, uploadAttachment } from "../lib/api";
import { describeError } from "../lib/errors";
import { formatDateTime, formatSessionSource, statusLabel, statusTone } from "../lib/format";
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

// asString 安全提取事件载荷中的字符串字段。
function asString(value: unknown): string | null {
  return typeof value === "string" && value.length > 0 ? value : null;
}

// StopNotice 是非正常结束的 Run 在对话内的提示配置。
type StopNotice = {
  // tone 决定配色：danger 用于失败，warning 用于中断或取消。
  tone: "danger" | "warning";
  // title 提示标题。
  title: string;
  // detail 具体原因，优先展示错误码与错误信息。
  detail: string;
  // actionLabel 运行详情入口文案。
  actionLabel: string;
  // allowRetry 是否允许基于上条消息重新编辑发送。
  allowRetry: boolean;
};

// stopNoticeFor 把任意非正常终止的 Run 映射为对话内提示。
// 只要不是 completed，就必须让用户看到"为什么停了"，不能依赖用户主动打开运行详情。
function stopNoticeFor(run: Run | null): StopNotice | null {
  if (!run || !isTerminal(run.status) || run.status === "completed") {
    return null;
  }
  const reason = [run.error_code, run.error_message].filter(Boolean).join(" · ");
  switch (run.status) {
    case "failed":
      return { tone: "danger", title: "本轮处理失败", detail: reason || "可以打开运行详情查看执行步骤。", actionLabel: "查看完整过程", allowRetry: true };
    case "reconciliation_required":
      return {
        tone: "warning", title: "本轮执行中断，需要人工对账",
        detail: reason || "工具执行结果未知，请前往运行详情确认后结案。",
        actionLabel: "查看并处理", allowRetry: false,
      };
    case "cancelled":
      return { tone: "warning", title: "本轮执行已停止", detail: reason || "本次执行已取消，可以编辑上条消息重新发送。", actionLabel: "查看完整过程", allowRetry: true };
    default:
      return { tone: "danger", title: "本轮执行异常终止", detail: reason || "请打开运行详情查看执行步骤。", actionLabel: "查看完整过程", allowRetry: true };
  }
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

interface TimelineEntry {
  key: string;
  label: string;
  detail?: string;
  tone: "info" | "success" | "danger" | "warning";
}

// TranscriptMessage 是持久化消息与临时流消息的统一视图模型。
interface TranscriptMessage {
  key: string;
  role: string;
  content: string;
  toolCalls?: unknown;
  name?: string;
  createdAt?: string;
  attachments?: Attachment[];
  // live 表示这条消息来自当前 Run 的临时流，尚未落库。
  live: boolean;
  // streaming 表示这条消息仍在增量输出。
  streaming: boolean;
}

// TranscriptBlock 是正文中的一个渲染块：普通消息或折叠的过程记录。
type TranscriptBlock =
  | { kind: "message"; key: string; message: TranscriptMessage }
  | { kind: "process"; key: string; items: TranscriptMessage[] };

// isProcessMessage 判断消息是否属于执行过程，正文只保留一行摘要。
function isProcessMessage(message: { role: string; toolCalls?: unknown }): boolean {
  return message.role === "tool" || (message.role === "assistant" && Array.isArray(message.toolCalls) && message.toolCalls.length > 0);
}

// processToolNames 汇总过程记录中的工具名称，用于运行中状态提示。
function processToolNames(items: TranscriptMessage[]): string[] {
  return [...new Set(items.flatMap((item) => (item.name ? [item.name] : toolCallNames(item.toolCalls))))];
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

// ProcessSummary 把一轮工具过程折叠成一行摘要，明细统一收进运行详情。
function ProcessSummary({ items, running, onOpen }: { items: TranscriptMessage[]; running: boolean; onOpen: () => void }) {
  const names = processToolNames(items);
  const preview = names.slice(0, 3).join("、");
  const extra = names.length > 3 ? ` 等 ${names.length} 个工具` : "";
  return (
    <div className="flex flex-wrap items-center gap-x-2 gap-y-1 rounded-lg border border-line/70 bg-paper-raised/45 px-3 py-2 text-xs text-muted">
      <Icon name="activity" className={cn("size-3.5 shrink-0", running && "text-accent")} />
      <span className="min-w-0 flex-1 break-words">
        {running ? `正在调用工具${preview ? `：${preview}${extra}` : "…"}` : `本轮处理过程 · ${items.length} 条记录`}
      </span>
      <Button size="sm" variant="ghost" className="px-1.5 text-xs text-accent" onClick={onOpen}>
        查看运行详情
      </Button>
    </div>
  );
}

export default function ChatPage() {
  const { sessionId = "" } = useParams();
  return <ChatWorkspace key={sessionId} sessionId={sessionId} />;
}

function ChatWorkspace({ sessionId }: { sessionId: string }) {
  const toast = useToast();
  const session = useRemote(() => requestJSON<Session>(`/api/v1/sessions/${sessionId}`), [sessionId]);
  const messages = useRemote(() => requestJSON<Page<Message>>(`/api/v1/sessions/${sessionId}/messages?limit=100`), [sessionId]);
  // 保留已归档助手，仅用于展示会话绑定的助手名称；发送能力仍要求已发布状态。
  const agents = useRemote(() => requestJSON<Page<Agent>>("/api/v1/agents?limit=100&include_archived=true"), []);
  const messagesReload = messages.reload;
  const messagesDataRef = useRef<Page<Message> | null>(null);
  useLayoutEffect(() => { messagesDataRef.current = messages.data; }, [messages.data]);
  // 长会话按游标继续加载更早消息，避免只显示最近 100 条。
  const [olderMessages, setOlderMessages] = useState<Message[]>([]);
  const [olderCursor, setOlderCursor] = useState<string | null>(null);
  const [olderEnded, setOlderEnded] = useState(false);
  const [olderLoading, setOlderLoading] = useState(false);
  const [runsVersion, setRunsVersion] = useState(0);
  const runs = useRemote(() => requestJSON<Page<Run>>(`/api/v1/sessions/${sessionId}/runs?limit=20`), [sessionId, runsVersion]);

  const [draft, setDraft] = useState("");
  const [replacementDraft, setReplacementDraft] = useState<string | null>(null);
  const [sending, setSending] = useState(false);
  const [sendError, setSendError] = useState<string | null>(null);
  const [pendingAttachments, setPendingAttachments] = useState<PendingAttachment[]>([]);
  const pendingAttachmentsRef = useRef<PendingAttachment[]>([]);
  const composerRef = useRef<HTMLTextAreaElement>(null);
  const sendingRef = useRef(false);
  const messageRequest = useRef<{ content: string; attachmentIDs: string[]; key: string } | null>(null);
  const [pendingRun, setPendingRun] = useState<{ messageID: string; key: string } | null>(null);
  const [sentMessages, setSentMessages] = useState<Message[]>([]);
  const [liveUntilReload, setLiveUntilReload] = useState<{ page: Page<Message> | null } | null>(null);
  const fileInputRef = useRef<HTMLInputElement | null>(null);
  // removedAttachmentKeys 记录上传完成前已被用户移除的文件，用于回收服务端对象。
  const removedAttachmentKeys = useRef<Set<string>>(new Set());
  const [activeRun, setActiveRun] = useState<Run | null>(null);
  const activeRunID = useRef<string | null>(null);
  useLayoutEffect(() => { activeRunID.current = activeRun?.id ?? null; }, [activeRun?.id]);
  const [liveMessages, setLiveMessages] = useState<LiveMessage[]>([]);
  const [timeline, setTimeline] = useState<TimelineEntry[]>([]);
  const [connection, setConnection] = useState<"idle" | "connecting" | "streaming" | "reconnecting" | "closed">("idle");
  const [streamError, setStreamError] = useState<string | null>(null);
  const [streamAttempt, setStreamAttempt] = useState(0);
  const streamCursor = useRef({ runID: "", seq: 0 });
  const [approvals, setApprovals] = useState<ToolApproval[]>([]);
  const [deciding, setDeciding] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [copiedKey, setCopiedKey] = useState<string | null>(null);
  const [showJump, setShowJump] = useState(false);
  const [wideReading, setWideReading] = useState(false);
  const [inspectorOpen, setInspectorOpen] = useState(false);
  const inspectorRef = useRef<HTMLDialogElement>(null);
  const inspectorToggleRef = useRef<HTMLButtonElement>(null);
  const inspectorBackdropPress = useRef(false);
  const [cancelling, setCancelling] = useState(false);
  const abortRef = useRef<AbortController | null>(null);
  const messageListRef = useRef<HTMLDivElement>(null);
  const transcriptRef = useRef<HTMLDivElement>(null);
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

  const canRun = session.data?.status === "active" && agent?.status === "published";
  const canCompose = canRun && !sending && !pendingRun;
  const canAttach = canCompose && pendingAttachments.length < MAX_ATTACHMENTS_PER_MESSAGE;
  const runActive = activeRun !== null && !isTerminal(activeRun.status);

  const refreshApprovals = useCallback(async (runID: string) => {
    try {
      const page = await requestJSON<{ items: ToolApproval[] }>(`/api/v1/runs/${runID}/approvals`);
      if (activeRunID.current === runID) setApprovals(page.items);
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
    if (streamCursor.current.runID !== runID) {
      streamCursor.current = { runID, seq: 0 };
      setLiveMessages([]);
      setLiveUntilReload(null);
      setTimeline([]);
      setApprovals([]);
    }
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
        case "step.started": {
          const worker = asString(payload.worker);
          pushTimeline({
            key: `${event.seq}:${event.event}`,
            label: `第 ${String(payload.step ?? "?")} 轮开始${worker ? `（${worker}）` : ""}`,
            tone: "info",
          });
          break;
        }
        case "step.completed": {
          const worker = asString(payload.worker);
          pushTimeline({
            key: `${event.seq}:${event.event}`,
            label: `第 ${String(payload.step ?? "?")} 轮完成${worker ? `（${worker}）` : ""}`,
            detail: `输入 ${String(payload.input_tokens ?? 0)} / 输出 ${String(payload.output_tokens ?? 0)} Token`,
            tone: "success",
          });
          break;
        }
        case "tool.started": {
          const worker = asString(payload.worker);
          pushTimeline({
            key: `${event.seq}:${event.event}`,
            label: `调用工具 ${asString(payload.tool) ?? ""}${worker ? `（${worker}）` : ""}`,
            detail: asString(payload.risk_level) ?? undefined,
            tone: "info",
          });
          break;
        }
        case "tool.approval.required":
          pushTimeline({ key: `${event.seq}:${event.event}`, label: `等待人工审批：${asString(payload.tool) ?? ""}`, tone: "warning" });
          void refreshApprovals(runID);
          break;
        case "tool.approval.decided":
          pushTimeline({ key: `${event.seq}:${event.event}`, label: `审批已${payload.decision === "deny" ? "拒绝" : "通过"}`, tone: payload.decision === "deny" ? "danger" : "success" });
          void refreshApprovals(runID);
          break;
        case "tool.completed": {
          const worker = asString(payload.worker);
          pushTimeline({
            key: `${event.seq}:${event.event}`,
            label: `工具 ${asString(payload.tool) ?? ""} ${String(payload.status ?? "")}${worker ? `（${worker}）` : ""}`,
            detail: asString(payload.error) ?? undefined,
            tone: payload.status === "completed" ? "success" : "warning",
          });
          break;
        }
        case "worker.failed": {
          const status = asString(payload.status);
          const transcript = Array.isArray(payload.transcript) ? payload.transcript.length : 0;
          pushTimeline({
            key: `${event.seq}:${event.event}`,
            label: `Worker ${asString(payload.worker) ?? ""}${status === "cancelled" ? " 已取消" : " 失败"}`,
            detail: [asString(payload.code), asString(payload.message), transcript > 0 ? `保留 ${transcript} 条过程消息` : ""].filter(Boolean).join(" · ") || undefined,
            tone: status === "cancelled" ? "warning" : "danger",
          });
          break;
        }
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
        let truncated = false;
        try {
          await streamRunEvents(runID, {
            signal: controller.signal,
            lastEventId: streamCursor.current.seq,
            onOpen: () => {
              setConnection("streaming");
              setStreamError(null);
            },
            onEvent: (event) => {
              if (cancelled) return;
              // 服务端按连接时长主动截断事件流：这不是错误，也不能按序号去重丢弃。
              // 记录提示后立即用最后游标续订，且不消耗重连次数。
              if (event.event === "stream.truncated") {
                truncated = true;
                setStreamError(null);
                pushTimeline({
                  key: `${event.seq}:${event.event}:${Date.now()}`,
                  label: "事件流达到服务端时长上限，正在自动续订",
                  detail: `已接收至序号 ${String(event.data?.last_seq ?? "?")}`,
                  tone: "warning",
                });
                return;
              }
              // 事件读取/状态读取的瞬时故障不是 Run 失败，保留游标并自动重连。
              if (event.event === "stream.error") {
                setStreamError(null);
                pushTimeline({
                  key: `${event.seq}:${event.event}:${Date.now()}`,
                  label: "事件流暂时中断，正在自动重连",
                  detail: `${String(event.data?.reason ?? "unknown")} · 已接收至序号 ${String(event.data?.last_seq ?? "?")}`,
                  tone: "warning",
                });
                return;
              }
              if (event.seq > 0) {
                if (event.seq <= streamCursor.current.seq) return;
                streamCursor.current.seq = event.seq;
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
          if (cancelled || controller.signal.aborted) return;
          setActiveRun(current);
          if (isTerminal(current.status)) {
            setConnection("closed");
            setStreamError(null);
            // 保留临时流直到持久化消息返回，避免回复在刷新时消失。
            setLiveUntilReload({ page: messagesDataRef.current });
            messagesReload();
            setRunsVersion((value) => value + 1);
            return;
          }
        } catch (statusFailure) {
          setStreamError(describeError(statusFailure));
        }
        if (truncated) {
          // 主动截断不是失败：重置重试预算，短暂等待后用最后游标继续订阅。
          setConnection("reconnecting");
          attempt = -1;
          await sleep(1000, controller.signal);
          continue;
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
  }, [activeRun?.id, messagesReload, refreshApprovals, streamAttempt]);

  const updateAttachments = (update: (items: PendingAttachment[]) => PendingAttachment[]) => {
    const next = update(pendingAttachmentsRef.current);
    pendingAttachmentsRef.current = next;
    setPendingAttachments(next);
  };

  // 一次性预留整批附件的名额，文件选择和粘贴并发时也遵守数量限制。
  const uploadFiles = async (files: File[]) => {
    if (!canCompose || sendingRef.current) return;
    const room = MAX_ATTACHMENTS_PER_MESSAGE - pendingAttachmentsRef.current.length;
    const rejected: string[] = [];
    const batch = files.filter((file) => {
      if (!file.size || file.size > MAX_ATTACHMENT_BYTES) {
        rejected.push(`${file.name}（${file.size ? "超过 20 MB" : "空文件"}）`);
        return false;
      }
      return true;
    }).slice(0, room).map((file) => ({ file, key: crypto.randomUUID() }));

    const countExceeded = files.length - rejected.length > room;
    setSendError([
      rejected.length ? `未上传：${rejected.join("、")}` : null,
      countExceeded ? `每条消息最多添加 ${MAX_ATTACHMENTS_PER_MESSAGE} 个附件。` : null,
    ].filter(Boolean).join(" ") || null);

    updateAttachments((current) => [...current, ...batch.map(({ file, key }) => ({
      key, name: file.name, size: file.size, status: "uploading" as const,
    }))]);
    for (const { file, key } of batch) {
      if (removedAttachmentKeys.current.delete(key)) continue;
      try {
        const attachment = await uploadAttachment<Attachment>(`/api/v1/sessions/${sessionId}/attachments`, file);
        if (removedAttachmentKeys.current.delete(key)) {
          await requestJSON<void>(`/api/v1/attachments/${encodeURIComponent(attachment.id)}`, { method: "DELETE" });
        } else {
          updateAttachments((current) => current.map((item) => item.key === key ? { ...item, status: "ready", id: attachment.id } : item));
        }
      } catch (failure) {
        updateAttachments((current) => current.map((item) => item.key === key ? { ...item, status: "failed", error: describeError(failure) } : item));
      }
    }
  };

  const removePendingAttachment = async (key: string) => {
    const target = pendingAttachmentsRef.current.find((item) => item.key === key);
    if (target?.status === "uploading") removedAttachmentKeys.current.add(key);
    updateAttachments((current) => current.filter((item) => item.key !== key));
    if (target?.id) {
      try {
        await requestJSON<void>(`/api/v1/attachments/${encodeURIComponent(target.id)}`, { method: "DELETE" });
      } catch (failure) {
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

  const send = async () => {
    if (sendingRef.current || runActive || !canRun) return;
    const attachments = pendingAttachmentsRef.current;
    const attachmentIDs = attachments.flatMap((item) => item.status === "ready" && item.id ? [item.id] : []);
    const content = draft.trim();
    if (!pendingRun && !content && attachments.length === 0) return;
    if (!pendingRun && attachments.some((item) => item.status !== "ready")) {
      setSendError("请等待上传完成，或移除上传失败的附件。");
      return;
    }
    sendingRef.current = true;
    setSending(true);
    setSendError(null);
    setActionError(null);
    stickToBottomRef.current = true;
    setShowJump(false);
    try {
      let runRequest = pendingRun;
      if (!runRequest) {
        const messageContent = content || "我上传了附件，请结合附件内容回答。";
        let request = messageRequest.current;
        if (!request || request.content !== messageContent || request.attachmentIDs.join() !== attachmentIDs.join()) {
          request = { content: messageContent, attachmentIDs, key: crypto.randomUUID() };
          messageRequest.current = request;
        }
        const message = await mutate<Message>(`/api/v1/sessions/${sessionId}/messages`, "POST", {
          content: request.content,
          attachment_ids: request.attachmentIDs,
        }, request.key);
        messageRequest.current = null;
        setSentMessages((current) => [...current, message]);
        setDraft("");
        updateAttachments(() => []);
        runRequest = { messageID: message.id, key: crypto.randomUUID() };
        setPendingRun(runRequest);
        messagesReload();
      }

      const run = await mutate<Run>(`/api/v1/sessions/${sessionId}/runs`, "POST", {
        agent_id: agent?.id ?? null,
        agent_version: agent?.version ?? null,
        mode: agent?.mode ?? "react",
        trigger_message_id: runRequest.messageID,
      }, runRequest.key);
      setPendingRun(null);
      setActiveRun(run);
    } catch (failure) {
      setSendError(describeError(failure));
    } finally {
      sendingRef.current = false;
      setSending(false);
    }
  };

  const cancel = async () => {
    if (!activeRun || cancelling) return;
    setCancelling(true);
    setActionError(null);
    try {
      const updated = await mutate<Run>(`/api/v1/runs/${activeRun.id}/cancel`, "POST", undefined, crypto.randomUUID());
      setActiveRun(updated);
      if (isTerminal(updated.status)) {
        abortRef.current?.abort();
        setConnection("closed");
        setStreamError(null);
        setLiveUntilReload({ page: messagesDataRef.current });
        messagesReload();
        setRunsVersion((value) => value + 1);
      }
    } catch (failure) {
      setActionError(describeError(failure));
    } finally {
      setCancelling(false);
    }
  };

  const decide = async (approval: ToolApproval, decision: "approve" | "deny") => {
    if (!activeRun || deciding) return;
    setDeciding(approval.id);
    setActionError(null);
    try {
      await mutate<ToolApproval>(`/api/v1/runs/${activeRun.id}/approvals/${approval.id}`, "POST", { decision }, crypto.randomUUID());
      await refreshApprovals(activeRun.id);
      const current = await requestJSON<Run>(`/api/v1/runs/${activeRun.id}`);
      if (activeRunID.current !== activeRun.id) return;
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

  const pendingApprovals = runActive ? approvals.filter((approval) => approval.status === "pending") : [];
  const persistedMessages = useMemo(() => {
    const unique = new Map<string, Message>();
    for (const message of [...olderMessages, ...(messages.data?.items ?? []).slice().reverse()]) {
      unique.set(message.id, message);
    }
    for (const message of sentMessages) {
      if (!unique.has(message.id)) unique.set(message.id, message);
    }
    return [...unique.values()];
  }, [olderMessages, messages.data, sentMessages]);

  useEffect(() => {
    if (!messages.data) return;
    const savedIDs = new Set(messages.data.items.map((message) => message.id));
    setSentMessages((current) => current.some((message) => savedIDs.has(message.id))
      ? current.filter((message) => !savedIDs.has(message.id))
      : current);
  }, [messages.data]);
  const liveVisible = liveUntilReload === null || liveUntilReload.page === messages.data ? liveMessages : [];
  // transcriptMessages 把持久化消息与临时流消息归一化，保持原有时间顺序。
  const transcriptMessages = useMemo<TranscriptMessage[]>(() => [
    ...persistedMessages.map((message) => ({
      key: message.id,
      role: message.role,
      content: message.content,
      toolCalls: message.tool_calls ?? undefined,
      createdAt: message.created_at,
      attachments: message.attachments,
      live: false,
      streaming: false,
    })),
    ...liveVisible.map((message) => ({
      key: message.key,
      role: message.role,
      content: message.content,
      toolCalls: message.toolCalls,
      name: message.name,
      live: true,
      streaming: message.key.startsWith("delta:"),
    })),
  ], [persistedMessages, liveVisible]);
  // transcriptBlocks 把连续的过程记录合并成一行摘要，正文只保留对话内容。
  const transcriptBlocks = useMemo<TranscriptBlock[]>(() => {
    const blocks: TranscriptBlock[] = [];
    let group: TranscriptMessage[] = [];
    const flush = () => {
      if (!group.length) return;
      blocks.push({ kind: "process", key: `process:${group[0].key}`, items: group });
      group = [];
    };
    for (const message of transcriptMessages) {
      if (isProcessMessage(message)) {
        group.push(message);
        continue;
      }
      flush();
      blocks.push({ kind: "message", key: message.key, message });
    }
    flush();
    return blocks;
  }, [transcriptMessages]);
  // currentProcessItems 取最近一轮用户输入之后的全部过程记录，供运行详情展示。
  const currentProcessItems = useMemo(() => {
    let start = 0;
    transcriptMessages.forEach((message, index) => {
      if (message.role === "user") start = index;
    });
    return transcriptMessages.slice(start + 1).filter(isProcessMessage);
  }, [transcriptMessages]);
  const hasMessages = persistedMessages.length + liveVisible.length > 0;
  const lastUserContent = persistedMessages.slice().reverse().find((message) => message.role === "user")?.content;
  const latestRun = runs.data?.items[0] ?? null;
  const settledRun = !runActive ? (activeRun ?? latestRun) : null;
  const stopNotice = stopNoticeFor(settledRun);

  useLayoutEffect(() => {
    const composer = composerRef.current;
    if (!composer) return;
    composer.style.height = "auto";
    composer.style.height = `${composer.scrollHeight}px`;
  }, [draft]);

  // 内容变化、窗口缩放和输入框增高都只在贴近底部时继续跟随。
  useLayoutEffect(() => {
    const list = messageListRef.current;
    const transcript = transcriptRef.current;
    if (!list || !transcript) return;
    const keepAtBottom = () => {
      if (stickToBottomRef.current) list.scrollTop = list.scrollHeight;
    };
    keepAtBottom();
    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(keepAtBottom);
    observer.observe(list);
    observer.observe(transcript);
    return () => observer.disconnect();
  }, []);

  useLayoutEffect(() => {
    const list = messageListRef.current;
    if (list && stickToBottomRef.current) list.scrollTop = list.scrollHeight;
  }, [persistedMessages, liveMessages, runActive, pendingApprovals.length]);

  useLayoutEffect(() => {
    const inspector = inspectorRef.current;
    if (!inspectorOpen || !inspector) return;
    inspector.showModal();
    return () => {
      inspector.close();
      inspectorToggleRef.current?.focus({ preventScroll: true });
    };
  }, [inspectorOpen]);

  const scrollToLatest = () => {
    const list = messageListRef.current;
    if (!list) return;
    stickToBottomRef.current = true;
    list.scrollTop = list.scrollHeight;
    setShowJump(false);
    list.focus({ preventScroll: true });
  };

  const onMessageScroll = () => {
    const list = messageListRef.current;
    if (!list) return;
    const nearBottom = list.scrollHeight - list.scrollTop - list.clientHeight <= 56;
    stickToBottomRef.current = nearBottom;
    setShowJump(!nearBottom && hasMessages);
  };

  const editMessage = (content: string) => {
    if (draft.trim() && draft !== content) {
      setReplacementDraft(content);
      return;
    }
    setDraft(content);
    composerRef.current?.focus({ preventScroll: true });
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

  const threadWidth = wideReading ? "max-w-[92rem]" : "max-w-[70rem]";
  const approvalNeeded = pendingApprovals.length > 0;
  const sendDisabled = !canRun || sending || (!pendingRun && (
    pendingAttachments.some((item) => item.status !== "ready")
    || (!draft.trim() && pendingAttachments.length === 0)
  ));
  const composerPlaceholder = session.loading || agents.loading
    ? "正在加载对话…"
    : session.data?.status === "archived"
      ? "此对话已归档"
      : !canRun
        ? "当前没有可用的助手"
        : pendingRun
          ? "消息已保存，重试启动后可继续"
          : "给助手发消息…";

  return (
    <section className="flex min-h-0 flex-1 flex-col" aria-label="会话">
      <header className="flex min-h-14 shrink-0 items-center gap-2 border-b border-line/80 px-3 py-1.5 sm:gap-3 sm:px-6">
        <ActionLink to="/sessions" variant="ghost" size="sm" aria-label="返回对话列表" title="返回对话列表" className="size-10 shrink-0 px-0">
          <Icon name="back" />
        </ActionLink>
        <div className="min-w-0 flex-1">
          <h1 className="truncate text-base tracking-tight text-ink sm:text-lg">{session.data?.title ?? (session.loading ? "加载对话…" : "对话")}</h1>
          {session.data ? (
            <p className="truncate text-xs text-muted">
              {[agent?.name, agent ? modeLabel(agent.mode) : null, formatSessionSource(session.data.source),
                session.data.status !== "active" ? statusLabel(session.data.status) : null].filter(Boolean).join(" · ")}
            </p>
          ) : null}
        </div>
        <Button size="sm" variant="ghost" className="shrink-0 max-md:hidden" aria-pressed={wideReading} onClick={() => setWideReading((wide) => !wide)}>
          <Icon name={wideReading ? "shrink" : "expand"} className="size-4" />
          {wideReading ? "恢复宽度" : "宽屏阅读"}
        </Button>
        <Button ref={inspectorToggleRef} size="sm" variant="secondary" className="shrink-0 gap-1.5 px-2.5" aria-haspopup="dialog" aria-expanded={inspectorOpen} onClick={() => setInspectorOpen(true)}>
          <Icon name="panel" className="size-4" />
          <span className="sr-only sm:not-sr-only">运行详情</span>
          {runs.data?.items.length ? <span aria-hidden className="rounded bg-paper-deep px-1.5 text-xs tabular-nums text-muted">{runs.data.items.length}</span> : null}
        </Button>
      </header>

      <div className="relative flex min-h-0 flex-1 flex-col">
        <div
          ref={messageListRef}
          className="min-h-0 flex-1 overflow-y-auto overscroll-contain focus:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-accent/60"
          role="log"
          aria-label="消息记录"
          tabIndex={0}
          onScroll={onMessageScroll}
        >
          <div ref={transcriptRef} className={cn("mx-auto flex min-h-full w-full min-w-0 flex-col gap-6 px-4 py-5 sm:gap-8 sm:px-8 sm:py-8", threadWidth)}>
            {session.error ? <ErrorBanner message={session.error} onRetry={session.reload} /> : null}
            {agents.error ? <ErrorBanner message={agents.error} onRetry={agents.reload} /> : null}
            {messages.error ? <ErrorBanner message={messages.error} onRetry={messages.reload} /> : null}
            {!hasMessages && !messages.error && !session.error ? (
              messages.loading || session.loading ? <LoadingState label="加载消息" /> : (
                <div className="my-auto flex flex-col items-center gap-4 py-5 text-center sm:gap-5">
                  <span className="grid size-14 place-items-center rounded-2xl border border-accent/25 bg-accent-soft text-accent">
                    <Icon name="agent" className="size-7" />
                  </span>
                  <h2 className="text-2xl tracking-tight sm:text-3xl">{canRun ? "你想完成什么？" : "还没有消息"}</h2>
                  {canRun ? (
                    <div className="flex max-w-lg flex-wrap justify-center gap-2">
                      {["帮我梳理这段需求", "把材料拆成待办", "解释一下这个报错"].map((prompt) => (
                        <Button key={prompt} size="sm" variant="secondary" onClick={() => editMessage(prompt)}>
                          {prompt}<Icon name="arrow" className="size-3.5 text-muted" />
                        </Button>
                      ))}
                    </div>
                  ) : !agents.loading ? (
                    <p className="text-sm text-muted">{session.data?.status === "archived" ? "已归档的对话仅供查看。" : "选择已发布的助手后，即可开始对话。"}</p>
                  ) : null}
                </div>
              )
            ) : null}

            {!olderEnded && (olderCursor || messages.data?.next_cursor) ? (
              <div className="flex justify-center">
                <Button size="sm" variant="secondary" loading={olderLoading} onClick={() => void loadOlderMessages()}>
                  加载更早消息
                </Button>
              </div>
            ) : null}
            {transcriptBlocks.map((block) => (block.kind === "process" ? (
              <ProcessSummary
                key={block.key}
                items={block.items}
                running={runActive && block.items.some((item) => item.live)}
                onOpen={() => setInspectorOpen(true)}
              />
            ) : (
              <MessageBubble
                key={block.key}
                messageKey={block.message.key}
                role={block.message.role}
                content={block.message.content}
                attachments={block.message.attachments}
                onDownloadAttachment={downloadMessageAttachment}
                createdAt={block.message.createdAt}
                live={runActive && block.message.streaming}
                copied={copiedKey === block.message.key}
                onCopy={copyMessage}
                onEdit={canCompose && !block.message.live && block.message.role === "user" && block.message.content
                  ? () => editMessage(block.message.content)
                  : undefined}
              />
            )))}

            {stopNotice && settledRun ? (
              <div
                role="alert"
                aria-live="assertive"
                className={cn("rounded-xl border p-4", stopNotice.tone === "danger" ? "border-rust/40 bg-rust-soft/65" : "border-ochre/40 bg-ochre-soft/45")}
              >
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <p className={cn("font-medium", stopNotice.tone === "danger" ? "text-rust" : "text-ochre")}>{stopNotice.title}</p>
                  <Link to={`/runs/${settledRun.id}`} className="rounded text-sm text-accent underline underline-offset-2">{stopNotice.actionLabel}</Link>
                </div>
                <p className={cn("mt-1.5 break-words text-sm leading-6", stopNotice.tone === "danger" ? "text-rust" : "text-ink-soft")}>
                  {stopNotice.detail}
                </p>
                {stopNotice.allowRetry && lastUserContent ? (
                  <Button size="sm" variant="secondary" className="mt-3" onClick={() => editMessage(lastUserContent)} disabled={!canCompose}>
                    编辑上条消息
                  </Button>
                ) : null}
              </div>
            ) : null}

            {approvalNeeded ? (
              <section aria-label="待审批操作" className="rounded-xl border border-ochre/35 bg-ochre-soft/45 p-4">
                <h2 className="text-sm font-semibold text-ochre">需要确认的操作（{pendingApprovals.length}）</h2>
                <div className="mt-3 space-y-3">
                  {pendingApprovals.map((approval) => {
                    const fields = approvalFields(approval.arguments);
                    return (
                      <div key={approval.id} className="rounded-lg border border-line bg-paper-raised p-3">
                        <div className="flex flex-wrap items-center justify-between gap-2">
                          <p className="break-all text-sm font-medium">{approval.tool_name}</p>
                          <Badge tone={approval.risk_level === "high" ? "danger" : "warning"}>{approval.risk_level === "high" ? "高风险" : approval.risk_level === "medium" ? "需留意" : "低风险"}</Badge>
                        </div>
                        {fields.length ? (
                          <dl className="mt-3 max-h-40 space-y-2 overflow-auto rounded-lg bg-paper p-3 font-mono text-xs leading-5">
                            {fields.map(([key, value]) => (
                              <div key={key} className="grid gap-0.5 sm:grid-cols-[minmax(0,1fr)_minmax(0,3fr)] sm:gap-3">
                                <dt className="break-all text-muted">{key}</dt>
                                <dd className="break-all text-ink-soft">{value}</dd>
                              </div>
                            ))}
                          </dl>
                        ) : null}
                        <div className="mt-3 flex flex-wrap gap-2">
                          <Button size="sm" loading={deciding === approval.id} disabled={Boolean(deciding)} onClick={() => void decide(approval, "approve")}>批准并继续</Button>
                          <Button size="sm" variant="danger" disabled={Boolean(deciding)} onClick={() => void decide(approval, "deny")}>拒绝</Button>
                          {approval.arguments ? (
                            <Button size="sm" variant="ghost" onClick={() => void copyMessage(`approval-${approval.id}`, JSON.stringify(approval.arguments, null, 2))}>
                              {copiedKey === `approval-${approval.id}` ? "已复制" : "复制参数"}
                            </Button>
                          ) : null}
                        </div>
                      </div>
                    );
                  })}
                </div>
              </section>
            ) : null}
          </div>
        </div>
        {showJump ? (
          <Button size="sm" variant="secondary" onClick={scrollToLatest} className="absolute inset-x-0 bottom-3 z-10 mx-auto w-fit rounded-full shadow-lg">
            <Icon name="down" className="size-4" /> 回到最新消息
          </Button>
        ) : null}
      </div>

      <footer className={cn("chat-composer mx-auto flex w-full shrink-0 bg-paper px-3 pt-1 sm:px-8", threadWidth)}>
        <form
          onSubmit={(event) => {
            event.preventDefault();
            if (!sendDisabled && !runActive) void send();
          }}
          className="flex min-h-0 w-full flex-col rounded-2xl border border-line-strong bg-paper-raised p-2.5 shadow-lg transition-[border-color,box-shadow] focus-within:border-accent/55 focus-within:ring-2 focus-within:ring-accent/10 sm:p-3"
        >
          <div className="min-h-0 overflow-y-auto overscroll-contain px-0.5">
            {pendingRun && !sending ? (
              <div role="status" className="mb-2 flex flex-wrap items-center gap-x-3 gap-y-1 rounded-lg border border-ochre/30 bg-ochre-soft/50 px-3 py-1.5 text-sm text-ochre">
                <span className="min-w-0 flex-1">消息已保存，尚未启动执行。</span>
                <Button size="sm" variant="ghost" onClick={() => {
                  setPendingRun(null);
                  setSendError(null);
                  composerRef.current?.focus({ preventScroll: true });
                }}>继续编辑</Button>
              </div>
            ) : null}
            {sendError || actionError || streamError ? (
              <div role="alert" className="mb-2 flex max-h-24 items-start gap-2 overflow-auto rounded-lg border border-rust/30 bg-rust-soft px-3 py-2 text-sm text-rust">
                <span className="min-w-0 flex-1 break-words">{sendError || actionError || `连接中断 · ${streamError}`}</span>
                <button type="button" aria-label="关闭提示" onClick={() => { setSendError(null); setActionError(null); setStreamError(null); }} className="grid size-6 shrink-0 place-items-center rounded hover:bg-paper-deep">
                  <Icon name="close" className="size-4" />
                </button>
              </div>
            ) : null}
            <input
              ref={fileInputRef}
              type="file"
              multiple
              className="hidden"
              aria-label="选择附件"
              onChange={(event) => {
                const files = Array.from(event.target.files ?? []);
                event.target.value = "";
                if (files.length) void uploadFiles(files);
              }}
            />
            {pendingAttachments.length ? (
              <ul className="mb-1 flex max-h-[14dvh] flex-wrap gap-1.5 overflow-y-auto px-1 py-0.5" aria-label="待发送附件">
                {pendingAttachments.map((item) => (
                  <li key={item.key} className={cn("flex max-w-full items-center gap-1.5 rounded-lg border px-2 text-xs", item.status === "failed" ? "border-rust/30 bg-rust-soft text-rust" : "border-line bg-paper-deep text-ink-soft")}>
                    <Icon name="attach" className="size-4 shrink-0" />
                    <span className="max-w-40 truncate" title={item.name}>{item.name}</span>
                    <span className="shrink-0" role={item.status === "ready" ? undefined : "status"} title={item.error}>
                      {item.status === "uploading" ? "上传中…" : item.status === "failed" ? "上传失败" : formatFileSize(item.size)}
                    </span>
                    <Button size="sm" variant="ghost" disabled={sending} onClick={() => void removePendingAttachment(item.key)} className="min-h-9 px-1" aria-label={`移除附件 ${item.name}`}>
                      <Icon name="close" className="size-3.5" />
                    </Button>
                  </li>
                ))}
              </ul>
            ) : null}
            <textarea
              ref={composerRef}
              rows={1}
              value={draft}
              onChange={(event) => setDraft(event.target.value)}
              placeholder={composerPlaceholder}
              disabled={!canRun}
              readOnly={sending || Boolean(pendingRun)}
              className="block max-h-[min(12rem,24dvh)] min-h-10 w-full resize-none overflow-y-auto bg-transparent px-2 py-2 text-base leading-6 text-ink placeholder:text-muted focus:outline-none disabled:opacity-70"
              aria-label="消息内容"
              onPaste={(event) => {
                const files = Array.from(event.clipboardData.files);
                if (!files.length || !canCompose) return;
                if (!event.clipboardData.getData("text/plain")) event.preventDefault();
                void uploadFiles(files);
              }}
              onKeyDown={(event) => {
                if (event.nativeEvent.isComposing || event.keyCode === 229) return;
                // 触屏键盘保留 Enter 换行，发送使用右侧按钮。
                if (event.key !== "Enter" || event.shiftKey || runActive || window.matchMedia("(pointer: coarse)").matches) return;
                event.preventDefault();
                if (!sendDisabled) void send();
              }}
            />
          </div>
          <div className="mt-1 flex min-h-10 shrink-0 items-center gap-2 px-0.5">
            <Button size="sm" variant="ghost" className="size-10 shrink-0 px-0" aria-label="添加附件" title="添加附件 · 每个文件不超过 20 MB" disabled={!canAttach} onClick={() => fileInputRef.current?.click()}>
              <Icon name="plus" className="size-5" />
            </Button>
            {runActive ? (
              <p role="status" className="flex min-w-0 items-center gap-2 text-xs text-muted sm:text-sm">
                <span aria-hidden className="size-1.5 shrink-0 animate-pulse rounded-full bg-accent" />
                <span className="truncate">{approvalNeeded ? "等待审批" : connection === "reconnecting" ? "正在重连…" : connection === "closed" ? "执行中 · 实时连接已关闭" : "助手正在处理…"}</span>
              </p>
            ) : (
              <span className="hidden min-w-0 truncate text-xs text-muted sm:inline">{pendingRun ? "消息已保存" : "Enter 发送 · Shift + Enter 换行"}</span>
            )}
            <div className="ml-auto flex shrink-0 items-center gap-1">
              {runActive && connection === "closed" ? (
                <Button size="sm" variant="ghost" onClick={() => setStreamAttempt((value) => value + 1)}>重连</Button>
              ) : null}
              {runActive ? (
                <Button size="sm" variant="secondary" loading={cancelling} aria-label="停止本轮" title="停止本轮" className="size-10 rounded-xl px-0" onClick={() => void cancel()}>
                  {cancelling ? null : <Icon name="stop" className="size-5" />}
                </Button>
              ) : (
                <Button size="sm" type="submit" loading={sending} disabled={sendDisabled} aria-label={pendingRun ? "重试启动" : "发送"} title={pendingRun ? "重试启动" : "发送"} className={cn("h-10 rounded-xl", !pendingRun && "w-10 px-0")}>
                  {pendingRun ? "重试启动" : sending ? null : <Icon name="send" className="size-5" />}
                </Button>
              )}
            </div>
          </div>
        </form>
      </footer>

      {inspectorOpen ? (
        <dialog
          id="chat-inspector"
          ref={inspectorRef}
          aria-labelledby="chat-inspector-title"
          className="editor-drawer fixed inset-y-0 right-0 left-auto m-0 h-dvh max-h-none w-full max-w-2xl overflow-hidden border-line bg-paper text-ink shadow-2xl sm:border-l"
          onCancel={(event) => { event.preventDefault(); setInspectorOpen(false); }}
          onPointerDown={(event) => { inspectorBackdropPress.current = event.target === event.currentTarget; }}
          onClick={(event) => {
            if (inspectorBackdropPress.current && event.target === event.currentTarget) setInspectorOpen(false);
            inspectorBackdropPress.current = false;
          }}
        >
          <div className="flex h-full min-h-0 flex-col">
            <header className="flex shrink-0 items-center justify-between gap-3 border-b border-line px-5 py-3">
              <h2 id="chat-inspector-title" className="text-lg">运行详情</h2>
              <Button size="sm" variant="ghost" className="size-10 px-0" aria-label="关闭运行详情" autoFocus onClick={() => setInspectorOpen(false)}>
                <Icon name="close" />
              </Button>
            </header>
            <div className="min-h-0 flex-1 space-y-6 overflow-y-auto overscroll-contain p-5">
              {activeRun ? (
                <section aria-label="处理过程" className="space-y-3">
                  <h3 className="text-sm font-semibold">处理过程</h3>
                  <div className="flex flex-wrap items-center gap-2 text-sm">
                    <Badge tone={statusTone(activeRun.status)}>{statusLabel(activeRun.status)}</Badge>
                    <span className="text-muted">{modeLabel(activeRun.mode)} · 第 {activeRun.attempt_no} 次尝试</span>
                    <Link to={`/runs/${activeRun.id}`} className="ml-auto rounded text-xs text-accent underline underline-offset-2">查看完整过程</Link>
                  </div>
                  {activeRun.error_message ? <ErrorBanner message={activeRun.error_message} /> : null}
                  {timeline.length > 0 ? (
                    <ol className="space-y-3 border-l border-line pl-4" aria-label="处理时间线">
                      {timeline.map((entry) => (
                        <li key={entry.key} className="relative">
                          <span aria-hidden className={cn("absolute top-2 -left-[1.3rem] size-2 rounded-full ring-4 ring-paper", entry.tone === "danger" ? "bg-rust" : entry.tone === "warning" ? "bg-ochre" : "bg-accent")} />
                          <p className="break-words text-sm">{entry.label}</p>
                          {entry.detail ? <p className="mt-1 break-all text-xs leading-5 text-muted">{entry.detail}</p> : null}
                        </li>
                      ))}
                    </ol>
                  ) : <p className="text-sm text-muted">暂无过程事件</p>}
                </section>
              ) : null}
              <section aria-label="本轮过程记录" className="space-y-2">
                <h3 className="text-sm font-semibold">本轮工具调用与结果{currentProcessItems.length ? `（${currentProcessItems.length}）` : ""}</h3>
                {currentProcessItems.length ? (
                  <div className="space-y-2">
                    {currentProcessItems.map((item) => (
                      <ProcessRecord
                        key={item.key}
                        role={item.role}
                        content={item.content}
                        toolCalls={item.toolCalls}
                        name={item.name}
                        createdAt={item.createdAt}
                      />
                    ))}
                  </div>
                ) : <p className="text-sm text-muted">本轮没有工具调用记录。</p>}
              </section>
              <section aria-label="历史处理记录">
                <h3 className="mb-2 text-sm font-semibold">历史处理记录</h3>
                {runs.error ? <ErrorBanner message={runs.error} onRetry={runs.reload} /> : runs.loading && !runs.data ? <LoadingState /> : runs.data?.items.length ? (
                  <ul className="divide-y divide-line">
                    {runs.data.items.map((run) => (
                      <li key={run.id}>
                        <Link to={`/runs/${run.id}`} className="flex items-center gap-3 rounded-lg px-2 py-3 transition-colors hover:bg-paper-deep">
                          <Icon name="activity" className="size-4 shrink-0 text-muted" />
                          <span className="min-w-0 flex-1">
                            <span className="block text-sm">{modeLabel(run.mode)}</span>
                            <span className="block text-xs text-muted">{formatDateTime(run.created_at)}</span>
                          </span>
                          <Badge tone={statusTone(run.status)}>{statusLabel(run.status)}</Badge>
                          <Icon name="chevron" className="size-4 shrink-0 text-muted" />
                        </Link>
                      </li>
                    ))}
                  </ul>
                ) : <p className="text-sm text-muted">还没有处理记录</p>}
              </section>
            </div>
          </div>
        </dialog>
      ) : null}
      <ConfirmDialog
        open={replacementDraft !== null}
        title="替换输入框中的草稿？"
        description="选中的消息会载入输入框，你可以修改后再发送。"
        confirmLabel="替换草稿"
        tone="primary"
        onCancel={() => setReplacementDraft(null)}
        onConfirm={() => {
          if (replacementDraft !== null) setDraft(replacementDraft);
          setReplacementDraft(null);
          requestAnimationFrame(() => composerRef.current?.focus({ preventScroll: true }));
        }}
      />
    </section>
  );
}
