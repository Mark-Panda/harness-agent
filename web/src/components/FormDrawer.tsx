import { useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import type { FormEventHandler, ReactNode } from "react";
import { useBlocker } from "react-router-dom";
import { Icon } from "./Icon";
import { Button, cn } from "./ui";

interface FormDrawerProps {
  title: string;
  description?: string;
  submitLabel?: string;
  busy?: boolean;
  dirty?: boolean;
  submitDisabled?: boolean;
  error?: string | null;
  bodyClassName?: string;
  onSubmit: FormEventHandler<HTMLFormElement>;
  onCancel: () => void;
  onDiscardNavigation?: () => void;
  children: ReactNode;
}

// 抽屉滚动区与操作区独立，关闭后保留原列表位置和焦点。
export function FormDrawer({
  title, description, submitLabel, busy = false, dirty = false, submitDisabled = false,
  error, bodyClassName, onSubmit, onCancel, onDiscardNavigation, children,
}: FormDrawerProps) {
  const titleID = useId();
  const descriptionID = useId();
  const dialogRef = useRef<HTMLDialogElement>(null);
  const titleRef = useRef<HTMLHeadingElement>(null);
  const continueRef = useRef<HTMLButtonElement>(null);
  const previousFocus = useRef<HTMLElement | null>(null);
  const closing = useRef(false);
  const backdropPress = useRef(false);
  const [discard, setDiscard] = useState(false);
  const rememberFocus = () => {
    previousFocus.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  };
  const blocker = useBlocker(({ currentLocation, nextLocation }) => {
    if (!dirty || busy || closing.current || currentLocation.key === nextLocation.key) return false;
    rememberFocus();
    return true;
  });
  const confirming = discard || blocker.state === "blocked";

  useLayoutEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) return;
    const trigger = document.activeElement;
    dialog.showModal();
    const initial = dialog.querySelector<HTMLElement>('[data-initial-focus], input:not([type="hidden"]):not(:disabled), select:not(:disabled), textarea:not(:disabled)');
    (initial ?? titleRef.current)?.focus({ preventScroll: true });
    return () => {
      dialog.close();
      if (trigger instanceof HTMLElement && trigger.isConnected) trigger.focus({ preventScroll: true });
    };
  }, []);

  useEffect(() => {
    if (confirming) {
      continueRef.current?.focus();
    } else if (previousFocus.current) {
      const previous = previousFocus.current;
      const canFocus = previous.isConnected && dialogRef.current?.contains(previous) && !previous.matches(":disabled");
      (canFocus ? previous : titleRef.current)?.focus({ preventScroll: true });
      previousFocus.current = null;
    }
  }, [confirming]);

  useEffect(() => {
    if (!dirty) return;
    const warn = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      event.returnValue = "";
    };
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [dirty]);

  const cancelEdit = () => {
    closing.current = true;
    onCancel();
  };

  const requestClose = () => {
    if (busy) return;
    if (dirty) {
      rememberFocus();
      setDiscard(true);
    } else {
      cancelEdit();
    }
  };

  const continueEditing = () => {
    setDiscard(false);
    if (blocker.state === "blocked") blocker.reset();
  };

  const discardEdits = () => {
    if (blocker.state === "blocked") {
      onDiscardNavigation?.();
      blocker.proceed();
    } else {
      cancelEdit();
    }
  };

  return (
    <dialog
      ref={dialogRef}
      aria-labelledby={titleID}
      aria-describedby={description ? descriptionID : undefined}
      className="editor-drawer fixed inset-y-0 right-0 left-auto m-0 h-dvh max-h-none w-full max-w-[44rem] overflow-hidden bg-paper text-ink shadow-2xl sm:border-l sm:border-line-strong"
      onCancel={(event) => {
        event.preventDefault();
        if (confirming) continueEditing();
        else requestClose();
      }}
      onPointerDown={(event) => { backdropPress.current = event.target === event.currentTarget; }}
      onClick={(event) => {
        if (backdropPress.current && event.target === event.currentTarget && !confirming) requestClose();
        backdropPress.current = false;
      }}
    >
      <form
        className="flex h-full min-h-0 flex-col"
        aria-busy={busy || undefined}
        onSubmit={(event) => {
          if (busy || confirming || submitDisabled || !submitLabel) {
            event.preventDefault();
            return;
          }
          onSubmit(event);
        }}
      >
        <header className="flex shrink-0 items-start gap-4 border-b border-line bg-paper-raised px-5 py-4 sm:px-7 sm:py-5">
          <div className="min-w-0 flex-1">
            <h2 id={titleID} ref={titleRef} tabIndex={-1} className="break-words text-xl tracking-tight focus:outline-none sm:text-2xl">{title}</h2>
            {description ? <p id={descriptionID} className="mt-1.5 text-sm leading-6 text-muted">{description}</p> : null}
          </div>
          <Button size="sm" variant="ghost" className="size-10 shrink-0 p-0" aria-label="关闭编辑面板" onClick={requestClose} disabled={busy || confirming}>
            <Icon name="close" />
          </Button>
        </header>

        <div inert={confirming || busy} className="min-h-0 flex-1 overflow-y-auto overscroll-contain px-5 py-5 sm:px-7 sm:py-6">
          <fieldset disabled={busy || confirming} className={cn("grid min-w-0 content-start gap-x-5 gap-y-5", bodyClassName ?? "sm:grid-cols-2")}>
            {children}
          </fieldset>
        </div>

        <footer className="shrink-0 border-t border-line bg-paper-raised px-5 py-3.5 pb-[max(0.875rem,env(safe-area-inset-bottom))] sm:px-7">
          {confirming ? (
            <div className="flex flex-wrap items-center justify-between gap-3">
              <p role="alert" className="text-sm font-medium text-ochre">放弃未保存的修改？</p>
              <div className="ml-auto flex flex-wrap gap-2">
                <Button ref={continueRef} variant="secondary" onClick={continueEditing}>继续编辑</Button>
                <Button variant="danger" onClick={discardEdits}>放弃修改</Button>
              </div>
            </div>
          ) : (
            <>
              {error ? <p role="alert" className="mb-3 max-h-28 overflow-y-auto break-words rounded-lg border border-rust/35 bg-rust-soft px-3 py-2 text-sm text-rust">{error}</p> : null}
              <div className="flex flex-wrap items-center justify-between gap-2">
                {busy || dirty ? <p role="status" className="text-xs text-muted">{busy ? "正在保存…" : "有未保存的修改"}</p> : null}
                <div className="ml-auto flex flex-wrap gap-2">
                  <Button variant="secondary" onClick={requestClose} disabled={busy}>{submitLabel ? "取消" : "关闭"}</Button>
                  {submitLabel ? <Button type="submit" loading={busy} disabled={submitDisabled}>{submitLabel}</Button> : null}
                </div>
              </div>
            </>
          )}
        </footer>
      </form>
    </dialog>
  );
}
