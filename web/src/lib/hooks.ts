// 数据加载 Hook：统一 loading/error/reload 语义，供各页面复用。
import { useCallback, useEffect, useRef, useState } from "react";
import type { DependencyList } from "react";
import { describeError } from "./errors";

export interface RemoteState<T> {
  // data 为最近一次成功结果。
  data: T | null;
  // loading 表示请求是否进行中。
  loading: boolean;
  // error 为可读错误文案。
  error: string | null;
  // reload 重新发起请求。
  reload: () => void;
}

// useRemote 在依赖变化时加载远端数据，并忽略过期响应。
export function useRemote<T>(loader: () => Promise<T>, deps: DependencyList = []): RemoteState<T> {
  const [data, setData] = useState<T | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [ticket, setTicket] = useState(0);
  const latestRef = useRef(0);

  const reload = useCallback(() => setTicket((value) => value + 1), []);

  useEffect(() => {
    const current = ++latestRef.current;
    setLoading(true);
    setError(null);
    loader()
      .then((result) => {
        if (latestRef.current === current) {
          setData(result);
          setLoading(false);
        }
      })
      .catch((loadError: unknown) => {
        if (latestRef.current === current) {
          setError(describeError(loadError));
          setLoading(false);
        }
      });
    return () => {
      latestRef.current += 1;
    };
    // loader 由调用方按依赖闭包提供，依赖变化由 deps 显式声明。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, ticket]);

  return { data, loading, error, reload };
}

// ClientPage 是完整加载列表的前端分页状态。
export interface ClientPage<T> {
  // page 当前页码，从 1 开始。
  page: number;
  // totalPages 总页数，至少为 1。
  totalPages: number;
  // pageItems 当前页数据。
  pageItems: T[];
  // setPage 跳转到指定页。
  setPage: (page: number) => void;
  // reset 回到第一页。
  reset: () => void;
}

// useClientPage 对一次完整加载的列表做前端分页，数据收缩时自动回退页码。
export function useClientPage<T>(items: T[], pageSize = 10): ClientPage<T> {
  const [page, setPage] = useState(1);
  const totalPages = Math.max(1, Math.ceil(items.length / pageSize));
  const current = Math.min(page, totalPages);
  useEffect(() => {
    if (page > totalPages) {
      setPage(totalPages);
    }
  }, [page, totalPages]);
  return {
    page: current,
    totalPages,
    pageItems: items.slice((current - 1) * pageSize, current * pageSize),
    setPage,
    reset: () => setPage(1),
  };
}

// CursorStack 是游标分页的页码栈，后端返回 next_cursor 时才能前进。
export interface CursorStack {
  // cursor 当前页游标，首页为空。
  cursor: string | null;
  // page 当前页码，从 1 开始。
  page: number;
  // next 携带下一页游标前进；游标为空时不移动。
  next: (nextCursor: string | null) => void;
  // prev 返回上一页。
  prev: () => void;
  // reset 回到首页。
  reset: () => void;
}

// useCursorStack 维护游标分页位置，避免只靠下一页游标丢失返回路径。
export function useCursorStack(): CursorStack {
  const [cursors, setCursors] = useState<(string | null)[]>([null]);
  return {
    cursor: cursors[cursors.length - 1],
    page: cursors.length,
    next: (nextCursor) => {
      if (nextCursor) {
        setCursors((current) => [...current, nextCursor]);
      }
    },
    prev: () => setCursors((current) => (current.length > 1 ? current.slice(0, -1) : current)),
    reset: () => setCursors([null]),
  };
}

// useScrollToTop 在编辑表单出现时回到页面顶部，避免表单渲染在视口外。
export function useScrollToTop(active: boolean) {
  useEffect(() => {
    if (active) {
      window.scrollTo({ top: 0, behavior: "smooth" });
    }
  }, [active]);
}
