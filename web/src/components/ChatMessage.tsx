import { formatDateTime } from "../lib/format";
import type { Attachment } from "../lib/types";
import { Icon } from "./Icon";
import { SafeMarkdown } from "./SafeMarkdown";
import { Button } from "./ui";

// toolCallNames 从消息的工具调用记录中提取名称，用于过程摘要。
export function toolCallNames(value: unknown): string[] {
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

// formatFileSize 把字节数转换为易读大小。
export function formatFileSize(bytes: number): string {
  if (bytes < 1024) {
    return `${bytes} B`;
  }
  if (bytes < 1024 * 1024) {
    return `${(bytes / 1024).toFixed(1)} KB`;
  }
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

// MessageBubble 渲染正文消息；工具过程记录统一收进运行详情。
export function MessageBubble({
  messageKey, role, content, createdAt, attachments, onDownloadAttachment,
  live = false, copied, onCopy, onEdit,
}: {
  messageKey: string;
  role: string;
  content: string;
  createdAt?: string;
  attachments?: Attachment[];
  onDownloadAttachment?: (attachment: Attachment) => void;
  live?: boolean;
  copied: boolean;
  onCopy: (key: string, content: string) => Promise<void>;
  onEdit?: () => void;
}) {
  const copy = () => void onCopy(messageKey, content);
  const timestamp = createdAt ? formatDateTime(createdAt) : null;
  if (role === "system") {
    return <p className="mx-auto max-w-[90%] break-words rounded-lg bg-paper-raised px-3 py-2 text-center text-xs leading-5 text-muted">{content}</p>;
  }
  if (role === "user") {
    return (
      <article aria-label="你的消息" data-message-id={messageKey} className="ml-auto min-w-0 max-w-[94%] sm:max-w-[82%]">
        <div className="whitespace-pre-wrap rounded-2xl rounded-tr-md border border-accent/20 bg-accent-soft/85 px-4 py-3 text-base leading-relaxed wrap-anywhere sm:px-5">{content}</div>
        {attachments?.length ? (
          <ul className="mt-2 space-y-1.5">
            {attachments.map((attachment) => (
              <li key={attachment.id}>
                <button
                  type="button"
                  onClick={() => onDownloadAttachment?.(attachment)}
                  className="flex min-h-10 w-full items-center gap-2 rounded-lg border border-line bg-paper-raised px-3 text-left text-sm text-ink-soft transition-colors hover:border-accent/50 hover:text-accent"
                  aria-label={`下载附件 ${attachment.file_name}`}
                  title={attachment.text_extracted ? (attachment.text_truncated ? "已读取（截断）" : "已读取内容") : "仅保存原件"}
                >
                  <Icon name="attach" className="size-4 shrink-0" />
                  <span className="min-w-0 flex-1 truncate">{attachment.file_name}</span>
                  <span className="shrink-0 text-xs text-muted">{formatFileSize(attachment.size_bytes)}</span>
                </button>
              </li>
            ))}
          </ul>
        ) : null}
        <div className="mt-1 flex flex-wrap items-center justify-end gap-x-2 text-xs text-muted">
          {timestamp ? <time dateTime={createdAt}>{timestamp}</time> : null}
          {onEdit ? (
            <Button variant="ghost" size="sm" aria-label="编辑后重发此消息" title="编辑后重发" className="size-9 px-0" onClick={onEdit}>
              <Icon name="edit" className="size-4" />
            </Button>
          ) : null}
          {content ? <CopyMessageButton copied={copied} onClick={copy} /> : null}
        </div>
      </article>
    );
  }
  if (role === "tool") {
    return null;
  }
  return (
    <article aria-label="助手回复" data-message-id={messageKey} className="min-w-0">
      <header className="mb-3 flex flex-wrap items-center gap-x-2.5 gap-y-1 text-xs text-muted">
        <span className="grid size-8 shrink-0 place-items-center rounded-lg border border-accent/25 bg-accent-soft text-accent">
          <Icon name="agent" className="size-4" />
        </span>
        <span className="text-sm font-semibold text-ink">助手</span>
        {timestamp ? <time dateTime={createdAt}>{timestamp}</time> : null}
        {live ? <span className="text-accent">生成中</span> : null}
      </header>
      <div className="min-w-0 text-base leading-[1.8] text-ink sm:text-[1.0625rem]">
        {content ? <SafeMarkdown content={content} /> : <p className="text-sm text-muted">这一步没有文本输出。</p>}
      </div>
      {content && !live ? <div className="mt-2"><CopyMessageButton copied={copied} onClick={copy} /></div> : null}
    </article>
  );
}

// ProcessRecord 展示单条工具过程记录，在运行详情中按需展开。
export function ProcessRecord({
  role, content, toolCalls, name, createdAt,
}: {
  role: string;
  content: string;
  toolCalls?: unknown;
  name?: string;
  createdAt?: string;
}) {
  const toolNames = toolCallNames(toolCalls);
  const toolCount = Array.isArray(toolCalls) ? toolCalls.length : 0;
  const timestamp = createdAt ? formatDateTime(createdAt) : null;
  return (
    <details className="group rounded-lg border border-line bg-paper-raised/70">
      <summary className="flex min-h-10 list-none flex-wrap items-center gap-2 rounded-lg px-3 py-2 text-sm text-ink-soft transition-colors hover:bg-paper-deep [&::-webkit-details-marker]:hidden">
        <Icon name="activity" className="size-4 text-muted" />
        <span className="min-w-0 flex-1 wrap-anywhere">{role === "tool" ? `工具结果${name ? ` · ${name}` : ""}` : `工具调用 · ${toolNames.join("、") || `${toolCount} 项`}`}</span>
        {timestamp ? <time dateTime={createdAt} className="text-xs text-muted">{timestamp}</time> : null}
        <Icon name="chevronDown" className="size-4 text-muted transition-transform group-open:rotate-180" />
      </summary>
      <div className="border-t border-line px-3 py-3 leading-relaxed text-ink-soft">
        {role === "tool" ? (
          <pre className="max-h-72 overflow-auto whitespace-pre-wrap break-all font-mono text-sm" tabIndex={0}>{content}</pre>
        ) : content ? <SafeMarkdown content={content} /> : <p className="text-sm text-muted">这一步没有文本输出。</p>}
        {toolCount > 0 ? (
          <details className="mt-3 text-sm text-muted">
            <summary className="cursor-pointer">调用参数</summary>
            <pre className="mt-2 max-h-72 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-paper p-3 font-mono text-xs" tabIndex={0}>
              {JSON.stringify(toolCalls, null, 2)}
            </pre>
          </details>
        ) : null}
      </div>
    </details>
  );
}

function CopyMessageButton({ copied, onClick }: { copied: boolean; onClick: () => void }) {
  return (
    <Button variant="ghost" size="sm" onClick={onClick} aria-label={copied ? "已复制消息" : "复制消息"} title="复制消息" className="px-2 text-muted">
      <Icon name={copied ? "check" : "copy"} className="size-4" />
      <span className="sr-only" aria-live="polite">{copied ? "已复制" : ""}</span>
    </Button>
  );
}
