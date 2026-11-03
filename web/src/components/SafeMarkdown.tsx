// 安全 Markdown 渲染：默认不解析原始 HTML，链接与图片遵循同源和协议白名单。
import { useRef, useState } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import type { Components } from "react-markdown";

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
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    const text = ref.current?.innerText ?? "";
    try {
      await navigator.clipboard?.writeText(text);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      setCopied(false);
    }
  };
  return (
    <div className="group relative my-3">
      <pre ref={ref} className="overflow-x-auto rounded-lg border border-line bg-paper-raised p-4 text-xs leading-6 text-ink">
        {children}
      </pre>
      <button
        type="button"
        onClick={copy}
        className="absolute right-2 top-2 rounded border border-line-strong bg-paper-raised px-2 py-1 text-[11px] text-ink-soft opacity-0 transition focus:opacity-100 group-hover:opacity-100"
      >
        {copied ? "已复制" : "复制代码"}
      </button>
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
  code({ node: _node, className, children, ...props }) {
    const inline = !className?.includes("language-");
    if (inline) {
      return (
        <code {...props} className="rounded bg-paper-deep px-1 py-0.5 font-mono text-[0.85em] text-jade">
          {children}
        </code>
      );
    }
    return (
      <code {...props} className={className}>
        {children}
      </code>
    );
  },
  table({ node: _node, children }) {
    return (
      <div className="my-3 overflow-x-auto rounded-lg border border-line">
        <table className="w-full border-collapse text-xs">{children}</table>
      </div>
    );
  },
  th({ node: _node, children }) {
    return <th className="border-b border-line bg-paper-raised px-3 py-2 text-left font-medium text-ink">{children}</th>;
  },
  td({ node: _node, children }) {
    return <td className="border-b border-line/60 px-3 py-2 text-ink-soft">{children}</td>;
  },
  p({ node: _node, children }) {
    return <p className="my-2 leading-6 text-ink-soft">{children}</p>;
  },
  ul({ node: _node, children }) {
    return <ul className="my-2 list-disc space-y-1 pl-5 text-ink-soft">{children}</ul>;
  },
  ol({ node: _node, children }) {
    return <ol className="my-2 list-decimal space-y-1 pl-5 text-ink-soft">{children}</ol>;
  },
};

// SafeMarkdown 渲染消息正文；原始 HTML 始终按文本处理。
export function SafeMarkdown({ content }: { content: string }) {
  return (
    <div className="text-sm">
      <ReactMarkdown remarkPlugins={[remarkGfm]} components={components} skipHtml>
        {content}
      </ReactMarkdown>
    </div>
  );
}
