// 安全 Markdown 渲染：默认不解析原始 HTML，链接与图片遵循同源和协议白名单。
import { isValidElement, memo, useRef, useState } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import type { Components } from "react-markdown";
import { Icon } from "./Icon";

// safeHref 仅允许 http(s) 与 mailto 协议，其余链接不渲染为可点击元素。
function safeHref(href: string | undefined): string | null {
  if (!href) {
    return null;
  }
  try {
    const parsed = new URL(href, window.location.origin);
    if (parsed.protocol === "http:" || parsed.protocol === "https:" || parsed.protocol === "mailto:") {
      return parsed.toString();
    }
    return null;
  } catch {
    return null;
  }
}

// CodeBlock 提供只复制文本的按钮，避免把渲染后的富文本写入剪贴板。
function CodeBlock({ children }: { children?: React.ReactNode }) {
  const ref = useRef<HTMLPreElement>(null);
  const [feedback, setFeedback] = useState<"copied" | "failed" | null>(null);
  const language = isValidElement<{ className?: string }>(children)
    ? children.props.className?.match(/\blanguage-([\w+-]+)/)?.[1]
    : undefined;
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(ref.current?.textContent ?? "");
      setFeedback("copied");
    } catch {
      setFeedback("failed");
    }
    window.setTimeout(() => setFeedback(null), 1800);
  };
  return (
    <div className="overflow-hidden rounded-xl border border-line bg-paper-raised">
      <div className="flex min-h-10 items-center justify-between gap-2 border-b border-line bg-paper-deep/55 px-3">
        <span className="font-mono text-xs text-muted">{language ?? "代码"}</span>
        <button
          type="button"
          onClick={copy}
          className="inline-flex min-h-9 items-center gap-1.5 rounded-md px-2 text-xs text-ink-soft transition-colors hover:bg-paper-deep hover:text-accent"
        >
          <Icon name={feedback === "copied" ? "check" : "copy"} className="size-3.5" />
          <span aria-live="polite">{feedback === "copied" ? "已复制" : feedback === "failed" ? "复制失败" : "复制代码"}</span>
        </button>
      </div>
      <pre ref={ref} tabIndex={0} className="overflow-x-auto p-4 font-mono text-[0.875em] leading-relaxed text-ink">
        {children}
      </pre>
    </div>
  );
}

const components: Components = {
  a({ node: _node, href, children, ...props }) {
    const safe = safeHref(href);
    if (!safe) {
      return <span className="text-ink-soft underline decoration-dotted">{children}</span>;
    }
    const external = safe.startsWith("http");
    return (
      <a
        {...props}
        href={safe}
        target={external ? "_blank" : undefined}
        rel={external ? "noopener noreferrer" : undefined}
        className="text-accent underline underline-offset-2 hover:text-jade"
      >
        {children}
      </a>
    );
  },
  img({ node: _node, src, alt }) {
    const safe = typeof src === "string" ? safeHref(src) : null;
    if (!safe) {
      return <span className="text-xs text-muted">[图片：{alt ?? "未命名"}]</span>;
    }
    return (
      <a href={safe} target="_blank" rel="noopener noreferrer" className="text-xs text-accent underline">
        查看图片：{alt ?? safe}
      </a>
    );
  },
  pre({ node: _node, children }) {
    return <CodeBlock>{children}</CodeBlock>;
  },
  table({ node: _node, children }) {
    return (
      <div className="overflow-x-auto rounded-xl border border-line">
        <table className="w-full border-collapse text-[0.875em]">{children}</table>
      </div>
    );
  },
  th({ node: _node, children }) {
    return <th className="border-b border-line bg-paper-raised px-3 py-2 text-left font-medium text-ink">{children}</th>;
  },
  td({ node: _node, children }) {
    return <td className="border-b border-line/60 px-3 py-2 text-ink-soft">{children}</td>;
  },
};

// SafeMarkdown 渲染消息正文；原始 HTML 始终按文本处理。
export const SafeMarkdown = memo(function SafeMarkdown({ content }: { content: string }) {
  return (
    <div className="chat-markdown min-w-0">
      <ReactMarkdown remarkPlugins={[remarkGfm]} components={components} skipHtml>
        {content}
      </ReactMarkdown>
    </div>
  );
});
