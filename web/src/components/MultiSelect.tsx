import { useEffect, useId, useRef, useState } from "react";
import type { ReactNode } from "react";
import { Icon } from "./Icon";
import { Input } from "./ui";

interface Option {
  id: string;
  label: string;
  detail?: string;
  badge?: ReactNode;
}

export function MultiSelect({ label, options, value, onChange, empty }: {
  label: string;
  options: Option[];
  value: string[];
  onChange: (value: string[]) => void;
  empty: string;
}) {
  const id = useId();
  const trigger = useRef<HTMLButtonElement>(null);
  const popover = useRef<HTMLDivElement>(null);
  const searchInput = useRef<HTMLInputElement>(null);
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const selected = new Set(value);
  const available = new Map(options.map((option) => [option.id, option]));
  const selection = value.map((key) => available.get(key)?.label ?? `已绑定项 ${key.slice(0, 8)}`);
  const retained = value.filter((key) => !available.has(key)).map((key) => ({
    id: key, label: `已绑定项 ${key.slice(0, 8)}`, detail: "未出现在可选列表中，可保留或移除", badge: undefined,
  }));
  const all = [...options, ...retained];
  const search = query.trim().toLocaleLowerCase();
  const visible = all.filter((option) => [option.label, option.detail].some((text) => text?.toLocaleLowerCase().includes(search)));

  const place = () => {
    const button = trigger.current;
    const panel = popover.current;
    if (!button || !panel) return;
    const { left, bottom, top, width } = button.getBoundingClientRect();
    const viewport = window.visualViewport;
    const viewportTop = viewport?.offsetTop ?? 0;
    const viewportBottom = viewportTop + (viewport?.height ?? window.innerHeight);
    const below = viewportBottom - bottom - 14;
    const above = top - viewportTop - 14;
    if (bottom < viewportTop || top > viewportBottom || Math.max(below, above) < 84) {
      if (panel.matches(":popover-open")) panel.hidePopover();
      return;
    }
    const upward = below < 250 && above > below;
    Object.assign(panel.style, {
      left: `${left}px`,
      width: `${width}px`,
      top: upward ? "auto" : `${bottom + 6}px`,
      bottom: upward ? `${window.innerHeight - top + 6}px` : "auto",
      maxHeight: `${Math.min(320, upward ? above : below)}px`,
    });
  };

  useEffect(() => {
    if (!open) return;
    const viewport = window.visualViewport;
    window.addEventListener("resize", place);
    window.addEventListener("scroll", place, { passive: true });
    viewport?.addEventListener("resize", place);
    viewport?.addEventListener("scroll", place);
    return () => {
      window.removeEventListener("resize", place);
      window.removeEventListener("scroll", place);
      viewport?.removeEventListener("resize", place);
      viewport?.removeEventListener("scroll", place);
    };
  }, [open]);

  return (
    <div className="relative min-w-0">
      <p id={`${id}-label`} className="mb-1.5 text-sm font-medium">{label}</p>
      <button
        ref={trigger}
        type="button"
        popoverTarget={id}
        disabled={!all.length}
        aria-expanded={open}
        aria-controls={id}
        aria-labelledby={`${id}-label ${id}-value`}
        className="form-control flex min-h-11 w-full min-w-0 items-center gap-2 rounded-lg px-3 py-2 text-left text-sm aria-expanded:border-accent aria-expanded:ring-2 aria-expanded:ring-accent/15"
      >
        <span id={`${id}-value`} className="flex min-w-0 flex-1 items-center gap-2">
          {selection.length ? (
            <>
              <span className="shrink-0 rounded-md bg-accent-soft px-2 py-0.5 text-xs text-accent">已选 {selection.length} 项</span>
              <span className="truncate text-ink-soft" title={selection.join("、")}>{selection.join("、")}</span>
            </>
          ) : <span className="truncate text-muted">{all.length ? `选择${label}` : empty}</span>}
        </span>
        <Icon name="chevronDown" className="size-4 shrink-0 text-muted" />
      </button>

      <div
        id={id}
        ref={popover}
        popover="auto"
        role="group"
        aria-labelledby={`${id}-label`}
        className="capability-popover fixed inset-auto m-0 overflow-hidden rounded-xl border border-line-strong bg-paper-raised text-ink shadow-2xl"
        onBeforeToggle={(event) => {
          if (event.newState === "open") place();
        }}
        onToggle={(event) => {
          const expanded = event.newState === "open";
          setOpen(expanded);
          if (expanded) {
            place();
            if (event.currentTarget.matches(":popover-open")) searchInput.current?.focus({ preventScroll: true });
          } else {
            setQuery("");
          }
        }}
        onKeyDown={(event) => {
          if (event.key !== "Escape" || event.nativeEvent.isComposing) return;
          event.preventDefault();
          event.stopPropagation();
          event.currentTarget.hidePopover();
          trigger.current?.focus({ preventScroll: true });
        }}
        onBlur={(event) => {
          const next = event.relatedTarget;
          if (next instanceof Node && next !== trigger.current && !event.currentTarget.contains(next)) {
            event.currentTarget.hidePopover();
          }
        }}
      >
        <div className="shrink-0 border-b border-line p-2">
          <Input
            ref={searchInput}
            type="search"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            aria-label={`搜索${label}`}
            placeholder={`搜索${label}…`}
          />
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto overscroll-contain p-1.5">
          {visible.length ? visible.map((option) => (
            <label key={option.id} className="flex min-h-11 cursor-pointer items-start gap-3 rounded-lg px-2.5 py-2 hover:bg-paper-deep focus-within:bg-paper-deep">
              <input
                type="checkbox"
                className="mt-0.5 size-4 shrink-0"
                checked={selected.has(option.id)}
                onChange={(event) => {
                  if (!event.target.checked && !available.has(option.id)) {
                    if (all.length === 1) popover.current?.hidePopover();
                    else searchInput.current?.focus({ preventScroll: true });
                  }
                  onChange(event.target.checked ? [...value, option.id] : value.filter((key) => key !== option.id));
                }}
              />
              <span className="min-w-0 flex-1">
                <span className="flex flex-wrap items-center gap-2">
                  <span className="text-sm font-medium wrap-anywhere">{option.label}</span>
                  {option.badge}
                </span>
                {option.detail ? <span className="mt-0.5 block text-xs leading-5 text-muted wrap-anywhere">{option.detail}</span> : null}
              </span>
            </label>
          )) : <p role="status" className="px-3 py-3 text-sm text-muted">没有匹配的选项</p>}
        </div>
      </div>
    </div>
  );
}
