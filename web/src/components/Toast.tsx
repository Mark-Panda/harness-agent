// 全局轻提示：操作成功后短时反馈，不遮挡主要内容。
import { createContext, useCallback, useContext, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { cn } from "./ui";

type ToastTone = "success" | "danger" | "info";

interface ToastItem {
  id: number;
  message: string;
  tone: ToastTone;
}

interface ToastAPI {
  show: (message: string, tone?: ToastTone) => void;
}

const ToastContext = createContext<ToastAPI | null>(null);

const toneClasses: Record<ToastTone, string> = {
  success: "border-jade bg-jade-soft text-jade",
  danger: "border-rust bg-rust-soft text-rust",
  info: "border-line-strong bg-paper-raised text-ink",
};

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([]);
  const nextID = useRef(0);

  const show = useCallback((message: string, tone: ToastTone = "success") => {
    const id = nextID.current += 1;
    // 最多同时显示三条，避免连续操作时堆满屏幕。
    setItems((current) => [...current.slice(-2), { id, message, tone }]);
    window.setTimeout(() => {
      setItems((current) => current.filter((item) => item.id !== id));
    }, 4000);
  }, []);

  const api = useMemo(() => ({ show }), [show]);

  return (
    <ToastContext.Provider value={api}>
      {children}
      <div aria-live="polite" className="pointer-events-none fixed inset-x-0 bottom-6 z-[60] flex flex-col items-center gap-2 px-4">
        {items.map((item) => (
          <div
            key={item.id}
            className={cn(
              "pointer-events-auto max-w-md rounded-[3px] border px-4 py-2 text-sm shadow-[0_10px_30px_rgb(31_27_21/0.18)]",
              toneClasses[item.tone],
            )}
          >
            {item.message}
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}

export function useToast(): ToastAPI {
  const context = useContext(ToastContext);
  if (!context) {
    throw new Error("useToast 必须在 ToastProvider 内使用");
  }
  return context;
}
