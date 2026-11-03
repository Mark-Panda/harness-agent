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
