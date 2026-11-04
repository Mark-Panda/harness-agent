// 纸墨风格 UI 原语：暖纸底、墨色文字、朱砂强调，统一焦点与可访问语义。
import type { ButtonHTMLAttributes, InputHTMLAttributes, ReactNode, SelectHTMLAttributes, TextareaHTMLAttributes } from "react";
import { useEffect, useId, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { NavLink } from "react-router-dom";

// cn 合并条件类名。
export function cn(...values: Array<string | false | null | undefined>): string {
  return values.filter(Boolean).join(" ");
}

type ButtonVariant = "primary" | "secondary" | "ghost" | "danger";
type ButtonSize = "sm" | "md";

const buttonVariants: Record<ButtonVariant, string> = {
  primary: "bg-ink text-paper-raised hover:bg-accent focus-visible:outline-accent",
  secondary: "border border-line-strong bg-paper-raised text-ink hover:border-accent hover:text-accent focus-visible:outline-accent",
  ghost: "text-ink-soft hover:bg-paper-deep hover:text-ink focus-visible:outline-accent",
  danger: "border border-rust bg-rust-soft/60 text-rust hover:bg-rust-soft focus-visible:outline-rust",
};

const buttonSizes: Record<ButtonSize, string> = {
  sm: "h-8 px-3 text-xs",
  md: "h-10 px-4 text-sm",
};

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
  size?: ButtonSize;
  loading?: boolean;
}

export function Button({ variant = "primary", size = "md", loading = false, className, children, disabled, ...rest }: ButtonProps) {
  return (
    <button
      {...rest}
      disabled={disabled || loading}
      className={cn(
        "inline-flex items-center justify-center gap-2 rounded-[3px] font-medium tracking-wide transition focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 disabled:cursor-not-allowed disabled:opacity-50",
        buttonVariants[variant],
        buttonSizes[size],
        className,
      )}
    >
      {loading ? <span aria-hidden className="size-3 animate-pulse rounded-full bg-current" /> : null}
      {children}
    </button>
  );
}

// Field 统一标签、说明与错误提示；hint 用浅墨色，错误用锈红。
export function Field({ label, hint, error, children }: { label: string; hint?: string; error?: string | null; children: ReactNode }) {
  return (
    <label className="flex flex-col gap-1.5 text-sm">
      <span className="font-medium text-ink">{label}</span>
      {children}
      {hint && !error ? <span className="text-xs leading-5 text-muted">{hint}</span> : null}
      {error ? (
        <span role="alert" className="text-xs leading-5 text-rust">
          {error}
        </span>
      ) : null}
    </label>
  );
}

// HelpTip 是紧贴标签的问号提示，鼠标悬停或键盘聚焦时显示解释。
export function HelpTip({ children }: { children: ReactNode }) {
  return (
    <span
      tabIndex={0}
      role="note"
      className="ml-1 inline-flex size-5 cursor-help items-center justify-center rounded-full border border-line-strong bg-paper-raised align-middle text-[11px] text-muted focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent"
      title={typeof children === "string" ? children : undefined}
    >
      ?
      <span className="sr-only">{children}</span>
    </span>
  );
}

const controlClass =
  "w-full rounded-[3px] border border-line-strong bg-paper-raised px-3 py-2 text-sm text-ink placeholder:text-muted/70 focus:border-accent focus:outline-none focus:ring-2 focus:ring-accent/20 disabled:opacity-60";

export function Input({ className, ...rest }: InputHTMLAttributes<HTMLInputElement>) {
  return <input {...rest} className={cn(controlClass, className)} />;
}

export function Textarea({ className, ...rest }: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return <textarea {...rest} className={cn(controlClass, "min-h-24 resize-y", className)} />;
}

export function Select({ className, ...rest }: SelectHTMLAttributes<HTMLSelectElement>) {
  return <select {...rest} className={cn(controlClass, className)} />;
}

type BadgeTone = "neutral" | "success" | "warning" | "danger" | "info";

// 徽章做成印章：小字号、加字距、硬边框，避免圆角药丸的通用感。
const badgeTones: Record<BadgeTone, string> = {
  neutral: "border-line-strong bg-paper-deep text-ink-soft",
  success: "border-jade bg-jade-soft text-jade",
  warning: "border-ochre bg-ochre-soft text-ochre",
  danger: "border-rust bg-rust-soft text-rust",
  info: "border-jade bg-jade-soft text-jade",
};

export function Badge({ tone = "neutral", children }: { tone?: BadgeTone; children: ReactNode }) {
  return (
    <span className={cn("inline-flex items-center rounded-[2px] border px-1.5 py-0.5 text-[11px] font-medium tracking-wide", badgeTones[tone])}>
      {children}
    </span>
  );
}

export function Card({ title, description, actions, children, className }: { title?: string; description?: string; actions?: ReactNode; children: ReactNode; className?: string }) {
  return (
    <section className={cn("rounded-[4px] border border-line bg-paper-raised shadow-[0_1px_0_rgb(31_27_21/0.04)]", className)}>
      {(title || actions) && (
        <header className="flex flex-wrap items-start justify-between gap-3 border-b border-line px-5 py-4">
          <div>
            {title ? <h2 className="text-lg text-ink">{title}</h2> : null}
            {description ? <p className="mt-1 max-w-2xl text-xs leading-5 text-muted">{description}</p> : null}
          </div>
          {actions ? <div className="flex flex-wrap items-center gap-2">{actions}</div> : null}
        </header>
      )}
      <div className="p-5">{children}</div>
    </section>
  );
}

// PageHeader 用朱砂短线做“栏目引子”，标题使用宋体，强化编辑排版感。
export function PageHeader({ eyebrow, title, description, actions }: { eyebrow?: string; title: string; description?: string; actions?: ReactNode }) {
  return (
    <div className="flex flex-wrap items-end justify-between gap-4 border-b border-line pb-5">
      <div>
        {eyebrow ? (
          <p className="flex items-center gap-2 text-[11px] font-semibold tracking-[0.18em] text-accent">
            <span aria-hidden className="h-px w-6 bg-accent" />
            {eyebrow}
          </p>
        ) : null}
        <h1 className="mt-2 text-3xl leading-tight text-ink">{title}</h1>
        {description ? <p className="mt-2 max-w-2xl text-sm leading-6 text-muted">{description}</p> : null}
      </div>
      {actions ? <div className="flex flex-wrap items-center gap-2">{actions}</div> : null}
    </div>
  );
}

// Callout 用左侧粗线表达语气，适合解释规则、风险与下一步。
export function Callout({ tone = "info", title, children }: { tone?: "info" | "warning" | "danger" | "success"; title?: string; children: ReactNode }) {
  const tones = {
    info: "border-jade bg-jade-soft/50",
    warning: "border-ochre bg-ochre-soft/60",
    danger: "border-rust bg-rust-soft/60",
    success: "border-jade bg-jade-soft/60",
  } as const;
  return (
    <div className={cn("rounded-[3px] border-l-4 px-4 py-3 text-sm leading-6 text-ink-soft", tones[tone])}>
      {title ? <p className="mb-1 font-medium text-ink">{title}</p> : null}
      {children}
    </div>
  );
}

// ChoiceCard 用于向导中的单选项，整块可点，带标题、解释和可选徽章。
export function ChoiceCard({
  selected,
  title,
  description,
  badge,
  onSelect,
  disabled,
}: {
  selected: boolean;
  title: string;
  description: string;
  badge?: ReactNode;
  onSelect: () => void;
  disabled?: boolean;
}) {
  return (
    <button
      type="button"
      role="radio"
      aria-checked={selected}
      disabled={disabled}
      onClick={onSelect}
      className={cn(
        "group relative flex w-full flex-col gap-2 rounded-[3px] border p-4 text-left transition focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent disabled:cursor-not-allowed disabled:opacity-50",
        selected
          ? "border-accent bg-paper-raised shadow-[inset_3px_0_0_var(--color-accent)]"
          : "border-line bg-paper-raised/70 hover:border-line-strong hover:bg-paper-raised",
      )}
    >
      <span className="flex flex-wrap items-center gap-2">
        <span className="font-medium text-ink">{title}</span>
        {badge}
        {selected ? <span className="ml-auto text-[11px] font-medium tracking-wide text-accent">已选择</span> : null}
      </span>
      <span className="text-xs leading-5 text-muted">{description}</span>
    </button>
  );
}

export function EmptyState({ title, description, action }: { title: string; description?: string; action?: ReactNode }) {
  return (
    <div className="flex flex-col items-center gap-3 rounded-[3px] border border-dashed border-line-strong bg-paper-raised/60 px-6 py-12 text-center">
      <span aria-hidden className="flex size-10 items-center justify-center rounded-full border border-line-strong font-display text-lg text-accent">
        导
      </span>
      <p className="text-base text-ink">{title}</p>
      {description ? <p className="max-w-md text-xs leading-5 text-muted">{description}</p> : null}
      {action}
    </div>
  );
}

// Pager 为一次完整加载的列表提供页码导航。
export function Pager({
  page,
  totalPages,
  total,
  pageSize = 10,
  onChange,
}: {
  page: number;
  totalPages: number;
  total: number;
  pageSize?: number;
  onChange: (page: number) => void;
}) {
  if (total <= 0) {
    return null;
  }
  return (
    <div className="mt-4 flex flex-wrap items-center justify-between gap-2 border-t border-line pt-3 text-xs text-muted">
      <span>共 {total} 条 · 每页 {pageSize} 条</span>
      <div className="flex items-center gap-2">
        <Button size="sm" variant="secondary" disabled={page <= 1} onClick={() => onChange(page - 1)}>上一页</Button>
        <span>第 {page} / {totalPages} 页</span>
        <Button size="sm" variant="secondary" disabled={page >= totalPages} onClick={() => onChange(page + 1)}>下一页</Button>
      </div>
    </div>
  );
}

// CursorPager 为后端游标分页的列表提供前后翻页；不知道总页数时只展示当前页。
export function CursorPager({
  page,
  count,
  hasNext,
  onPrev,
  onNext,
}: {
  page: number;
  count: number;
  hasNext: boolean;
  onPrev: () => void;
  onNext: () => void;
}) {
  if (count <= 0) {
    return null;
  }
  return (
    <div className="mt-4 flex flex-wrap items-center justify-between gap-2 border-t border-line pt-3 text-xs text-muted">
      <span>本页 {count} 条</span>
      <div className="flex items-center gap-2">
        <Button size="sm" variant="secondary" disabled={page <= 1} onClick={onPrev}>上一页</Button>
        <span>第 {page} 页</span>
        <Button size="sm" variant="secondary" disabled={!hasNext} onClick={onNext}>下一页</Button>
      </div>
    </div>
  );
}

export function ErrorBanner({ message, onRetry }: { message: string; onRetry?: () => void }) {
  return (
    <div role="alert" className="flex flex-wrap items-center justify-between gap-3 rounded-[3px] border-l-4 border-rust bg-rust-soft/70 px-4 py-3 text-sm text-rust">
      <span>{message}</span>
      {onRetry ? (
        <Button size="sm" variant="secondary" onClick={onRetry}>
          重试
        </Button>
      ) : null}
    </div>
  );
}

export function LoadingState({ label = "加载中" }: { label?: string }) {
  return (
    <div role="status" className="flex items-center gap-3 rounded-[3px] border border-line bg-paper-raised px-4 py-6 text-sm text-muted">
      <span aria-hidden className="size-4 animate-spin rounded-full border-2 border-accent border-t-transparent" />
      {label}
    </div>
  );
}

export function ConfirmDialog({
  open,
  title,
  description,
  confirmLabel = "确认",
  tone = "danger",
  busy = false,
  onConfirm,
  onCancel,
  children,
}: {
  open: boolean;
  title: string;
  description?: string;
  confirmLabel?: string;
  tone?: "danger" | "primary";
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
  children?: ReactNode;
}) {
  const titleID = useId();
  const descriptionID = useId();
  const panelRef = useRef<HTMLDivElement | null>(null);
  const restoreRef = useRef<HTMLElement | null>(null);

  // 打开时记住原焦点，关闭后还回去；Tab 在弹窗内循环，避免焦点跑到背景页。
  useEffect(() => {
    if (!open) {
      return;
    }
    restoreRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    return () => {
      restoreRef.current?.focus();
    };
  }, [open]);

  if (!open) {
    return null;
  }
  // 请求进行中禁止关闭弹窗，避免失败原因还没看到就被用户误关。
  const requestCancel = () => {
    if (!busy) {
      onCancel();
    }
  };
  const trapFocus = (event: React.KeyboardEvent<HTMLDivElement>) => {
    if (event.key !== "Tab") {
      return;
    }
    const nodes = Array.from(panelRef.current?.querySelectorAll<HTMLElement>('button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])') ?? [])
      .filter((node) => !node.hasAttribute("disabled"));
    if (nodes.length === 0) {
      return;
    }
    const first = nodes[0];
    const last = nodes[nodes.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  };
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-ink/35 p-4" role="presentation" onClick={requestCancel}>
      <div
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleID}
        aria-describedby={description ? descriptionID : undefined}
        className="w-full max-w-md rounded-[4px] border border-line-strong bg-paper-raised p-6 shadow-[0_20px_60px_rgb(31_27_21/0.25)]"
        onClick={(event) => event.stopPropagation()}
        onKeyDown={(event) => {
          trapFocus(event);
          if (event.key === "Escape") {
            requestCancel();
          }
        }}
      >
        <h2 id={titleID} className="text-xl text-ink">
          {title}
        </h2>
        {description ? (
          <p id={descriptionID} className="mt-2 text-sm leading-6 text-muted">
            {description}
          </p>
        ) : null}
        {children ? <div className="mt-4">{children}</div> : null}
        <div className="mt-6 flex justify-end gap-2">
          <Button variant="secondary" onClick={requestCancel} disabled={busy} autoFocus>
            取消
          </Button>
          <Button variant={tone === "danger" ? "danger" : "primary"} onClick={onConfirm} loading={busy}>
            {confirmLabel}
          </Button>
        </div>
      </div>
    </div>
  );
}

// menuItems 返回面板内可聚焦的菜单项。
function menuItems(panel: HTMLDivElement | null): HTMLElement[] {
  return Array.from(panel?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? []);
}

// DropdownMenu 是统一的下拉菜单：点击外部、Escape 或选择菜单项后关闭，方向键在项间移动。
export function DropdownMenu({
  label,
  triggerLabel,
  trigger,
  indicator = false,
  align = "right",
  triggerClassName,
  panelClassName,
  children,
}: {
  label: string;
  triggerLabel?: string;
  trigger: ReactNode;
  indicator?: boolean;
  align?: "left" | "right";
  triggerClassName?: string;
  panelClassName?: string;
  children: ReactNode;
}) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement | null>(null);
  const triggerRef = useRef<HTMLButtonElement | null>(null);
  const panelRef = useRef<HTMLDivElement | null>(null);
  // focusTarget 记录“用键盘打开菜单”时焦点应该落到第一项还是最后一项。
  const [focusTarget, setFocusTarget] = useState<"first" | "last" | null>(null);
  // 面板通过 Portal 渲染到 body，避免被表格滚动容器裁切。
  const [position, setPosition] = useState<{ top?: number; bottom?: number; left?: number; right?: number } | null>(null);
  const panelID = useId();

  useEffect(() => {
    if (!open) {
      return;
    }
    // 等定位完成、面板真正挂载后再把焦点送到菜单项。
    const frame = window.requestAnimationFrame(() => {
      if (!focusTarget) {
        return;
      }
      const items = menuItems(panelRef.current);
      (focusTarget === "last" ? items[items.length - 1] : items[0])?.focus();
      setFocusTarget(null);
    });
    return () => window.cancelAnimationFrame(frame);
  }, [open, focusTarget]);

  // 收起时清掉旧坐标，避免再次打开时先在旧位置闪一下。
  useEffect(() => {
    if (!open) {
      setPosition(null);
    }
  }, [open]);

  // 跟随触发器定位；下方空间不足时向上展开。
  useEffect(() => {
    if (!open) {
      return;
    }
    const place = () => {
      const rect = triggerRef.current?.getBoundingClientRect();
      if (!rect) {
        return;
      }
      const alignStyle = align === "right"
        ? { right: Math.max(8, window.innerWidth - rect.right) }
        : { left: Math.max(8, rect.left) };
      if (window.innerHeight - rect.bottom < 320 && rect.top > 360) {
        setPosition({ ...alignStyle, bottom: window.innerHeight - rect.top + 8 });
      } else {
        setPosition({ ...alignStyle, top: rect.bottom + 8 });
      }
    };
    place();
    window.addEventListener("resize", place);
    window.addEventListener("scroll", place, true);
    return () => {
      window.removeEventListener("resize", place);
      window.removeEventListener("scroll", place, true);
    };
  }, [open, align]);

  useEffect(() => {
    if (!open) {
      return;
    }
    const onPointerDown = (event: PointerEvent) => {
      const target = event.target as Node;
      if (!rootRef.current?.contains(target) && !panelRef.current?.contains(target)) {
        setOpen(false);
      }
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        setOpen(false);
        triggerRef.current?.focus();
      }
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [open]);

  // 触发器上按上下方向键直接打开菜单，并把焦点送到菜单项。
  const handleTriggerKeyDown = (event: React.KeyboardEvent<HTMLButtonElement>) => {
    if (event.key !== "ArrowDown" && event.key !== "ArrowUp") {
      return;
    }
    event.preventDefault();
    setFocusTarget(event.key === "ArrowDown" ? "first" : "last");
    setOpen(true);
  };

  const handlePanelKeyDown = (event: React.KeyboardEvent<HTMLDivElement>) => {
    const items = menuItems(panelRef.current);
    if (items.length === 0) {
      return;
    }
    const current = items.indexOf(document.activeElement as HTMLElement);
    if (event.key === "ArrowDown") {
      event.preventDefault();
      items[(current + 1) % items.length]?.focus();
    } else if (event.key === "ArrowUp") {
      event.preventDefault();
      items[(current - 1 + items.length) % items.length]?.focus();
    } else if (event.key === "Home") {
      event.preventDefault();
      items[0]?.focus();
    } else if (event.key === "End") {
      event.preventDefault();
      items[items.length - 1]?.focus();
    } else if (event.key === "Tab") {
      // Tab 保留浏览器默认顺序，但先把菜单收起。
      setOpen(false);
    }
  };

  return (
    <div ref={rootRef} className="relative">
      <button
        type="button"
        ref={triggerRef}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? panelID : undefined}
        aria-label={triggerLabel}
        onClick={() => setOpen((value) => !value)}
        onKeyDown={handleTriggerKeyDown}
        className={cn(
          "inline-flex h-8 items-center justify-center gap-2 rounded-[3px] text-xs font-medium tracking-wide transition focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent",
          triggerClassName,
        )}
      >
        {trigger}
        {indicator ? <span aria-hidden className="text-[10px]">{open ? "▴" : "▾"}</span> : null}
      </button>
      {open && position
        ? createPortal(
            <div
              id={panelID}
              ref={panelRef}
              role="menu"
              aria-label={label}
              style={{ position: "fixed", ...position }}
              className={cn(
                "z-50 rounded-[3px] border border-line bg-paper-raised p-2 shadow-[0_16px_40px_rgb(31_27_21/0.18)]",
                panelClassName,
              )}
              onClick={(event) => {
                if ((event.target as HTMLElement).closest('[role="menuitem"]')) {
                  window.requestAnimationFrame(() => setOpen(false));
                }
              }}
              onKeyDown={handlePanelKeyDown}
            >
              {children}
            </div>,
            document.body,
          )
        : null}
    </div>
  );
}

// MenuItem 统一菜单项语义；to 存在时渲染为导航链接，否则渲染为命令按钮。
export function MenuItem({
  to,
  onClick,
  danger = false,
  disabled = false,
  className,
  children,
}: {
  to?: string;
  onClick?: () => void;
  danger?: boolean;
  disabled?: boolean;
  className?: string;
  children: ReactNode;
}) {
  const classes = cn(
    "block w-full rounded-[2px] px-2 py-2 text-left text-xs transition hover:bg-paper-deep focus-visible:bg-paper-deep focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent disabled:cursor-not-allowed disabled:opacity-50",
    danger ? "text-rust" : "text-ink",
    className,
  );
  if (to) {
    return (
      <NavLink
        to={to}
        role="menuitem"
        tabIndex={-1}
        onClick={onClick}
        className={({ isActive }) => cn(classes, isActive ? "bg-paper-deep" : "")}
      >
        {children}
      </NavLink>
    );
  }
  return (
    <button type="button" role="menuitem" tabIndex={-1} disabled={disabled} onClick={onClick} className={classes}>
      {children}
    </button>
  );
}
