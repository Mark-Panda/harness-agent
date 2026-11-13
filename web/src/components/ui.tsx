// 共用界面组件，统一操作样式、焦点与可访问语义。
import type { ComponentPropsWithRef, FormEvent, ReactNode, SelectHTMLAttributes, TextareaHTMLAttributes } from "react";
import { cloneElement, isValidElement, useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { Link, NavLink } from "react-router-dom";
import type { LinkProps } from "react-router-dom";
import { Icon } from "./Icon";

// cn 合并条件类名。
export function cn(...values: Array<string | false | null | undefined>): string {
  return values.filter(Boolean).join(" ");
}

type ButtonVariant = "primary" | "secondary" | "ghost" | "danger";
type ButtonSize = "sm" | "md";

const buttonVariants: Record<ButtonVariant, string> = {
  primary: "bg-accent text-accent-ink hover:bg-accent-hover focus-visible:outline-accent",
  secondary: "border border-line-strong bg-paper-raised text-ink hover:border-accent/65 hover:bg-paper-deep focus-visible:outline-accent",
  ghost: "text-ink-soft hover:bg-paper-deep hover:text-ink focus-visible:outline-accent",
  danger: "border border-rust bg-rust-soft/60 text-rust hover:bg-rust-soft focus-visible:outline-rust",
};

const buttonSizes: Record<ButtonSize, string> = {
  sm: "min-h-9 px-3 py-1.5 text-xs max-sm:min-h-11",
  md: "min-h-11 px-4 py-2 text-sm",
};

const buttonClass = "inline-flex items-center justify-center gap-2 rounded-lg font-semibold transition-[background-color,border-color,color,transform] duration-150 focus-visible:outline-2 focus-visible:outline-offset-2 active:scale-[0.98]";

export interface ButtonProps extends ComponentPropsWithRef<"button"> {
  variant?: ButtonVariant;
  size?: ButtonSize;
  loading?: boolean;
}

export function Button({ variant = "primary", size = "md", type = "button", loading = false, className, children, disabled, ...rest }: ButtonProps) {
  return (
    <button
      {...rest}
      type={type}
      aria-busy={loading || undefined}
      disabled={disabled || loading}
      className={cn(
        buttonClass,
        "disabled:cursor-not-allowed disabled:opacity-50",
        buttonVariants[variant],
        buttonSizes[size],
        className,
      )}
    >
      {loading ? <span aria-hidden className="size-3.5 shrink-0 animate-spin rounded-full border-2 border-current border-r-transparent" /> : null}
      {children}
    </button>
  );
}

export function ActionLink({ variant = "primary", size = "md", className, ...rest }: LinkProps & {
  variant?: ButtonVariant;
  size?: ButtonSize;
}) {
  return <Link {...rest} className={cn(buttonClass, buttonVariants[variant], buttonSizes[size], className)} />;
}

type FieldControl = {
  id?: string;
  required?: boolean;
  "aria-describedby"?: string;
  "aria-invalid"?: ComponentPropsWithRef<"input">["aria-invalid"];
};

function validationMessage(event: FormEvent<HTMLDivElement>): string {
  const input = event.target as HTMLInputElement;
  const state = input.validity;
  if (!state) return "";
  if (state.valueMissing) return "请填写此项。";
  if (state.typeMismatch && input.type === "url") return "请输入完整的网址，例如 https://example.com。";
  if (state.tooShort) return `至少需要 ${input.minLength} 个字符。`;
  if (state.rangeUnderflow) return `请输入大于或等于 ${input.min} 的数值。`;
  return input.validationMessage;
}

// Field 关联标签和说明，校验失败后在控件旁显示错误。
export function Field({ label, hint, error, children }: { label: string; hint?: string; error?: string | null; children: ReactNode }) {
  const generatedID = useId();
  const [validationError, setValidationError] = useState("");
  const control = isValidElement<FieldControl>(children) ? children : null;
  const id = control?.props.id ?? generatedID;
  const message = error || validationError || hint;
  const invalid = Boolean(error || validationError);
  const messageID = message ? `${generatedID}-hint` : undefined;
  const description = [control?.props["aria-describedby"], messageID].filter(Boolean).join(" ");
  return (
    <div className="flex min-w-0 flex-col gap-1.5 text-sm" onInvalid={(event) => setValidationError(validationMessage(event))} onChange={() => setValidationError("")}>
      <label htmlFor={id} className="font-medium text-ink">
        {label}
        {control?.props.required ? <span aria-hidden="true" className="ml-1 text-accent">*</span> : null}
      </label>
      {control ? cloneElement(control, {
        id,
        "aria-describedby": description || undefined,
        "aria-invalid": invalid || control.props["aria-invalid"],
      }) : children}
      {message ? <span id={messageID} role={invalid ? "alert" : undefined} className={cn("text-xs leading-5", invalid ? "text-rust" : "text-muted")}>{message}</span> : null}
    </div>
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

const controlClass = "form-control min-w-0 w-full rounded-lg px-3 py-2 text-base text-ink sm:text-sm";

export function Input({ className, type, disabled, ...rest }: ComponentPropsWithRef<"input">) {
  const [visible, setVisible] = useState(false);
  if (type !== "password") {
    return <input {...rest} type={type} disabled={disabled} className={cn(controlClass, className)} />;
  }
  return (
    <div className="relative min-w-0">
      <input {...rest} type={visible ? "text" : "password"} disabled={disabled} className={cn(controlClass, "pr-12", className)} />
      <button
        type="button"
        aria-label={visible ? "隐藏密码或密钥" : "显示密码或密钥"}
        aria-controls={rest.id}
        title={visible ? "隐藏内容" : "显示内容"}
        disabled={disabled}
        className="absolute inset-y-1 right-1 grid w-10 place-items-center rounded-md text-muted transition-colors hover:bg-paper-deep hover:text-ink disabled:opacity-50"
        onClick={() => setVisible((value) => !value)}
      >
        <Icon name={visible ? "eyeOff" : "eye"} />
      </button>
    </div>
  );
}

export function Textarea({ className, ...rest }: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return <textarea {...rest} className={cn(controlClass, "min-h-24 resize-y", className)} />;
}

export function Select({ className, ...rest }: SelectHTMLAttributes<HTMLSelectElement>) {
  return <select {...rest} className={cn(controlClass, "form-select", className)} />;
}

type BadgeTone = "neutral" | "success" | "warning" | "danger" | "info";

const badgeTones: Record<BadgeTone, string> = {
  neutral: "border-line-strong bg-paper-deep text-ink-soft",
  success: "border-jade/35 bg-jade-soft text-jade",
  warning: "border-ochre/35 bg-ochre-soft text-ochre",
  danger: "border-rust/35 bg-rust-soft text-rust",
  info: "border-jade/35 bg-jade-soft text-jade",
};

export function Badge({ tone = "neutral", children }: { tone?: BadgeTone; children: ReactNode }) {
  return (
    <span className={cn("inline-flex items-center rounded-full border px-2 py-0.5 text-xs font-medium", badgeTones[tone])}>
      {children}
    </span>
  );
}

export function Card({
  id, title, description, actions, children, className, collapsible = false, defaultOpen = true,
  open: controlledOpen, onOpenChange,
}: {
  id?: string;
  title?: string;
  description?: string;
  actions?: ReactNode;
  children: ReactNode;
  className?: string;
  collapsible?: boolean;
  defaultOpen?: boolean;
  // 传入 open 时由父组件控制展开状态，便于按钮点击后自动定位到折叠区域。
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
}) {
  const [internalOpen, setInternalOpen] = useState(defaultOpen);
  const open = controlledOpen ?? internalOpen;
  const expanded = !collapsible || open;
  const setOpen = (next: boolean) => {
    if (controlledOpen === undefined) {
      setInternalOpen(next);
    }
    onOpenChange?.(next);
  };
  return (
    <section id={id} className={cn("min-w-0 scroll-mt-20 rounded-xl border border-line bg-paper-raised", className)}>
      {(title || actions) && (
        <header className="flex min-h-16 flex-wrap items-center justify-between gap-3 border-b border-line px-4 py-3 sm:px-5">
          <div className="min-w-0">
            <h2 className="break-words text-base text-ink">
              {collapsible && title ? (
                <button
                  type="button"
                  aria-expanded={expanded}
                  onClick={() => setOpen(!open)}
                  className="flex items-center gap-2 rounded text-left hover:text-accent"
                >
                  {title}
                  <Icon name="chevronDown" className={cn("size-4 shrink-0 text-muted transition-transform", expanded && "rotate-180")} />
                </button>
              ) : title}
            </h2>
            {description ? <p className="mt-1 max-w-2xl text-xs leading-5 text-muted">{description}</p> : null}
          </div>
          {actions ? <div className="flex flex-wrap items-center gap-2">{actions}</div> : null}
        </header>
      )}
      {expanded ? <div className="p-4 sm:p-5">{children}</div> : null}
    </section>
  );
}

// PageHeader 统一标题、说明和页面级操作。
export function PageHeader({ eyebrow, title, description, actions }: { eyebrow?: string; title: string; description?: string; actions?: ReactNode }) {
  return (
    <div className="flex flex-wrap items-end justify-between gap-4">
      <div className="min-w-0">
        {eyebrow && eyebrow !== title ? (
          <p className="mb-2 text-xs font-medium text-accent">
            {eyebrow}
          </p>
        ) : null}
        <h1 className="break-words text-2xl leading-tight tracking-tight text-ink sm:text-3xl">{title}</h1>
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
    <div className={cn("rounded-lg border-l-4 px-4 py-3 text-sm leading-6 text-ink-soft", tones[tone])}>
      {title ? <p className="mb-1 font-medium text-ink">{title}</p> : null}
      {children}
    </div>
  );
}

// ChoiceCard 用于向导中的单选项，整块可点，带标题、解释和可选徽章。
export function ChoiceCard({
  name, selected, title, description, badge, onSelect, disabled,
}: {
  name: string;
  selected: boolean;
  title: string;
  description: string;
  badge?: ReactNode;
  onSelect: () => void;
  disabled?: boolean;
}) {
  const detailID = useId();
  return (
    <label className={cn("relative block min-w-0", disabled ? "cursor-not-allowed opacity-60" : "cursor-pointer")}>
      <input
        type="radio"
        name={name}
        checked={selected}
        disabled={disabled}
        aria-label={title}
        aria-describedby={detailID}
        onChange={onSelect}
        className="peer sr-only"
      />
      <span className={cn(
        "flex flex-col gap-2 rounded-xl border px-4 py-3 text-left transition-colors peer-focus-visible:outline-2 peer-focus-visible:outline-offset-2 peer-focus-visible:outline-accent",
        selected ? "border-accent/65 bg-accent-soft/55" : "border-line-strong bg-paper hover:bg-paper-deep",
      )}>
        <span className="flex flex-wrap items-center gap-2.5">
          <span className={cn("grid size-4 shrink-0 place-items-center rounded-full border", selected ? "border-accent" : "border-line-strong")}>
            {selected ? <span className="size-2 rounded-full bg-accent" /> : null}
          </span>
          <span className="font-medium text-ink">{title}</span>
          {badge}
        </span>
        <span id={detailID} className="pl-6 text-sm leading-6 text-muted">{description}</span>
      </span>
    </label>
  );
}

export function EmptyState({ title, description, action }: { title: string; description?: string; action?: ReactNode }) {
  return (
    <div className="flex flex-col items-center gap-3 rounded-xl border border-dashed border-line bg-paper/65 px-4 py-9 text-center">
      <span aria-hidden className="flex size-10 items-center justify-center rounded-xl border border-accent/25 bg-accent-soft text-accent">
        <Icon name="inbox" className="size-5" />
      </span>
      <p className="text-base text-ink">{title}</p>
      {description ? <p className="max-w-md text-xs leading-5 text-muted">{description}</p> : null}
      {action}
    </div>
  );
}

// Pager 为一次完整加载的列表提供页码导航，空列表也保留总数信息。
export function Pager({
  page,
  totalPages,
  total,
  pageSize = 10,
  busy = false,
  onChange,
}: {
  page: number;
  totalPages: number;
  total: number;
  pageSize?: number;
  busy?: boolean;
  onChange: (page: number) => void;
}) {
  const lastPage = Math.max(1, totalPages);
  return (
    <div className="mt-4 flex flex-wrap items-center justify-between gap-2 border-t border-line pt-3 text-xs text-muted" aria-busy={busy || undefined}>
      <span>共 {total} 条 · 每页 {pageSize} 条</span>
      <div className="flex flex-wrap items-center gap-1.5">
        <Button size="sm" variant="secondary" disabled={busy || page <= 1} onClick={() => onChange(1)}>首页</Button>
        <Button size="sm" variant="secondary" disabled={busy || page <= 1} onClick={() => onChange(page - 1)}>上一页</Button>
        <span className="px-1 tabular-nums">第 {page} / {lastPage} 页</span>
        <Button size="sm" variant="secondary" disabled={busy || page >= lastPage} onClick={() => onChange(page + 1)}>下一页</Button>
        <Button size="sm" variant="secondary" disabled={busy || page >= lastPage} onClick={() => onChange(lastPage)}>末页</Button>
      </div>
    </div>
  );
}

// CursorPager 为后端游标分页的列表提供翻页；可以一键回到首页。
export function CursorPager({
  page,
  count,
  hasNext,
  busy = false,
  onFirst,
  onPrev,
  onNext,
}: {
  page: number;
  count: number;
  hasNext: boolean;
  busy?: boolean;
  onFirst: () => void;
  onPrev: () => void;
  onNext: () => void;
}) {
  return (
    <div className="mt-4 flex flex-wrap items-center justify-between gap-2 border-t border-line pt-3 text-xs text-muted" aria-busy={busy || undefined}>
      <span>本页 {count} 条</span>
      <div className="flex flex-wrap items-center gap-1.5">
        <Button size="sm" variant="secondary" disabled={busy || page <= 1} onClick={onFirst}>首页</Button>
        <Button size="sm" variant="secondary" disabled={busy || page <= 1} onClick={onPrev}>上一页</Button>
        <span className="px-1 tabular-nums">第 {page} 页</span>
        <Button size="sm" variant="secondary" disabled={busy || !hasNext} onClick={onNext}>下一页</Button>
      </div>
    </div>
  );
}

export function ErrorBanner({ message, onRetry }: { message: string; onRetry?: () => void }) {
  return (
    <div role="alert" className="flex flex-wrap items-center justify-between gap-3 rounded-lg border-l-4 border-rust bg-rust-soft/70 px-4 py-3 text-sm text-rust">
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
    <div role="status" className="flex items-center gap-3 rounded-lg border border-line bg-paper-raised px-4 py-6 text-sm text-muted">
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
  const dialogRef = useRef<HTMLDialogElement>(null);
  const cancelRef = useRef<HTMLButtonElement>(null);
  const backdropPress = useRef(false);

  useLayoutEffect(() => {
    const dialog = dialogRef.current;
    if (!open || !dialog) return;
    const trigger = document.activeElement;
    dialog.showModal();
    cancelRef.current?.focus();
    return () => {
      dialog.close();
      if (trigger instanceof HTMLElement && trigger.isConnected) trigger.focus({ preventScroll: true });
    };
  }, [open]);

  if (!open) return null;
  const requestCancel = () => {
    if (!busy) onCancel();
  };

  return (
    <dialog
      ref={dialogRef}
      role="alertdialog"
      aria-labelledby={titleID}
      aria-describedby={description ? descriptionID : undefined}
      aria-busy={busy || undefined}
      className="confirm-dialog fixed inset-0 m-auto max-h-[calc(100dvh-2rem)] w-[calc(100%_-_2rem)] max-w-md rounded-2xl border border-line-strong bg-paper-raised text-ink shadow-2xl"
      onCancel={(event) => {
        event.preventDefault();
        requestCancel();
      }}
      onPointerDown={(event) => { backdropPress.current = event.target === event.currentTarget; }}
      onClick={(event) => {
        if (backdropPress.current && event.target === event.currentTarget) requestCancel();
        backdropPress.current = false;
      }}
    >
      <div className="p-5 sm:p-6">
        <h2 id={titleID} className="text-xl tracking-tight">{title}</h2>
        {description ? <p id={descriptionID} className="mt-2 text-sm leading-6 text-muted">{description}</p> : null}
        {children ? <div className="mt-4">{children}</div> : null}
        <div className="mt-6 flex flex-wrap justify-end gap-2">
          <Button ref={cancelRef} variant="secondary" onClick={requestCancel} disabled={busy}>取消</Button>
          <Button variant={tone === "danger" ? "danger" : "primary"} onClick={onConfirm} loading={busy}>{confirmLabel}</Button>
        </div>
      </div>
    </dialog>
  );
}

// menuItems 返回面板内可聚焦的菜单项。
function menuItems(panel: HTMLDivElement | null): HTMLElement[] {
  return Array.from(panel?.querySelectorAll<HTMLElement>('[role="menuitem"]:not(:disabled)') ?? []);
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
  const triggerRef = useRef<HTMLButtonElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const initialFocus = useRef<"first" | "last" | "panel">("panel");
  const search = useRef({ text: "", time: 0 });
  const panelID = useId();

  const close = (restoreFocus: boolean) => {
    if (restoreFocus) triggerRef.current?.focus({ preventScroll: true });
    setOpen(false);
  };

  useLayoutEffect(() => {
    const panel = panelRef.current;
    const trigger = triggerRef.current;
    if (!open || !panel || !trigger) return;
    const place = () => {
      const rect = trigger.getBoundingClientRect();
      if (rect.width > 0 && rect.height > 0 && (rect.bottom <= 0 || rect.top >= window.innerHeight || rect.right <= 0 || rect.left >= window.innerWidth)) {
        setOpen(false);
        return;
      }
      const gap = 8;
      const width = panel.offsetWidth;
      const desiredLeft = align === "right" ? rect.right - width : rect.left;
      const left = Math.max(gap, Math.min(desiredLeft, window.innerWidth - width - gap));
      const below = window.innerHeight - rect.bottom - gap * 2;
      const above = rect.top - gap * 2;
      const upward = panel.scrollHeight > below && above > below;
      panel.style.left = `${left}px`;
      panel.style.top = upward ? "auto" : `${rect.bottom + gap}px`;
      panel.style.bottom = upward ? `${window.innerHeight - rect.top + gap}px` : "auto";
      panel.style.maxHeight = `${Math.max(0, upward ? above : below)}px`;
      panel.style.setProperty("--menu-offset", upward ? "4px" : "-4px");
    };

    place();
    const items = menuItems(panel);
    const target = initialFocus.current === "first" ? items[0] : initialFocus.current === "last" ? items.at(-1) : panel;
    if (target && target !== panel) target.focus();
    else panel.focus({ preventScroll: true });
    search.current = { text: "", time: 0 };

    const onScroll = (event: Event) => {
      if (event.target instanceof Node && panel.contains(event.target)) return;
      place();
    };
    const onOutsidePress = (event: PointerEvent) => {
      if (event.target instanceof Node && !trigger.contains(event.target) && !panel.contains(event.target)) setOpen(false);
    };
    window.addEventListener("resize", place);
    window.addEventListener("scroll", onScroll, true);
    document.addEventListener("pointerdown", onOutsidePress);
    return () => {
      window.removeEventListener("resize", place);
      window.removeEventListener("scroll", onScroll, true);
      document.removeEventListener("pointerdown", onOutsidePress);
    };
  }, [open, align]);

  const handlePanelKeyDown = (event: React.KeyboardEvent<HTMLDivElement>) => {
    if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      close(true);
      return;
    }
    if (event.key === "Tab") {
      // 将 Tab 交回触发器所在位置，由浏览器继续原来的顺序。
      close(true);
      return;
    }
    if (event.nativeEvent.isComposing) return;
    const items = menuItems(panelRef.current);
    if (!items.length) return;
    const current = items.indexOf(document.activeElement as HTMLElement);
    let target: HTMLElement | undefined;
    switch (event.key) {
      case "ArrowDown": target = items[(current + 1) % items.length]; break;
      case "ArrowUp": target = items[current < 0 ? items.length - 1 : (current - 1 + items.length) % items.length]; break;
      case "Home": target = items[0]; break;
      case "End": target = items.at(-1); break;
      default: {
        if (event.key.length !== 1 || event.key === " " || event.ctrlKey || event.metaKey || event.altKey) return;
        const now = Date.now();
        const key = event.key.toLocaleLowerCase();
        const previous = now - search.current.time < 500 ? search.current.text : "";
        const query = previous === key ? key : previous + key;
        search.current = { text: query, time: now };
        const next = current + 1;
        const ordered = query.length === 1 ? [...items.slice(next), ...items.slice(0, next)] : items;
        target = ordered.find((item) => (item.getAttribute("aria-label") ?? item.textContent ?? "").trim().toLocaleLowerCase().startsWith(query));
      }
    }
    if (target) {
      event.preventDefault();
      target.focus();
    }
  };

  return (
    <div className="relative inline-flex">
      <button
        type="button"
        ref={triggerRef}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? panelID : undefined}
        aria-label={triggerLabel}
        onClick={(event) => {
          if (open) {
            close(true);
          } else {
            initialFocus.current = event.detail === 0 ? "first" : "panel";
            setOpen(true);
          }
        }}
        onKeyDown={(event) => {
          if (event.key !== "ArrowDown" && event.key !== "ArrowUp") return;
          event.preventDefault();
          initialFocus.current = event.key === "ArrowDown" ? "first" : "last";
          if (open) {
            const items = menuItems(panelRef.current);
            (initialFocus.current === "last" ? items.at(-1) : items[0])?.focus();
          } else {
            setOpen(true);
          }
        }}
        className={cn(
          "group inline-flex min-h-10 items-center justify-center gap-2 rounded-lg border border-transparent text-sm font-medium transition-[background-color,border-color,color,transform] duration-150 active:scale-[0.98] aria-expanded:border-accent/30 aria-expanded:bg-accent-soft aria-expanded:text-accent",
          triggerClassName,
        )}
      >
        {trigger}
        {indicator ? <Icon name="chevronDown" className="size-3.5 shrink-0 text-current transition-transform group-aria-expanded:rotate-180" /> : null}
      </button>
      {open ? createPortal(
        <div
          id={panelID}
          ref={panelRef}
          role="menu"
          tabIndex={-1}
          aria-label={label}
          className={cn("menu-panel fixed z-50 max-w-[calc(100vw-1rem)] overflow-y-auto overscroll-contain rounded-xl border border-line-strong bg-paper-raised p-1.5 shadow-xl focus:outline-none", panelClassName)}
          onClick={(event) => {
            const item = (event.target as Element).closest<HTMLElement>('[role="menuitem"]');
            if (item) {
              const staysOnPage = item.tagName === "BUTTON" || item.getAttribute("aria-current") === "page"
                || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey;
              close(staysOnPage);
            }
          }}
          onKeyDown={handlePanelKeyDown}
        >
          {children}
        </div>,
        document.body,
      ) : null}
    </div>
  );
}

// MenuItem 统一菜单项语义；to 存在时渲染为导航链接，否则渲染为命令按钮。
export function MenuItem({
  to,
  onClick,
  danger = false,
  disabled = false,
  icon,
  className,
  children,
}: {
  to?: string;
  onClick?: () => void;
  danger?: boolean;
  disabled?: boolean;
  icon?: React.ComponentProps<typeof Icon>["name"];
  className?: string;
  children: ReactNode;
}) {
  const classes = cn(
    "flex min-h-10 w-full items-center gap-2.5 rounded-lg px-2.5 py-2 text-left text-sm transition-colors hover:bg-paper-deep focus-visible:bg-paper-deep focus-visible:outline-2 focus-visible:outline-offset-[-2px] focus-visible:outline-accent disabled:cursor-not-allowed disabled:opacity-50",
    danger ? "text-rust" : "text-ink",
    className,
  );
  const content = <>{icon ? <Icon name={icon} className="size-4 shrink-0 text-current" /> : null}<span className="min-w-0 flex-1">{children}</span></>;
  if (to) {
    return (
      <NavLink
        to={to}
        role="menuitem"
        tabIndex={-1}
        onClick={onClick}
        className={({ isActive }) => cn(classes, isActive && "bg-accent-soft text-accent")}
      >
        {({ isActive }) => <>{content}{isActive ? <Icon name="check" className="size-4 shrink-0 text-accent" /> : null}</>}
      </NavLink>
    );
  }
  return (
    <button type="button" role="menuitem" tabIndex={-1} disabled={disabled} onClick={onClick} className={classes}>
      {content}
    </button>
  );
}

export function RowMenu({ name, children }: { name: string; children: ReactNode }) {
  const label = `更多操作：${name}`;
  return (
    <DropdownMenu
      label={label}
      triggerLabel={label}
      trigger={<Icon name="more" />}
      triggerClassName="size-10 text-ink-soft ring-1 ring-line ring-inset hover:bg-paper-deep"
      panelClassName="w-44"
    >
      {children}
    </DropdownMenu>
  );
}
