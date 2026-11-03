// 鉴权上下文：Access Token 只存于内存，刷新令牌依赖 HttpOnly Cookie。
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { ApiError, configureApi, readCookie, requestJSON } from "./api";
import type { AuthSession, User } from "./types";

// csrfCookieName 与后端保持一致的 CSRF Cookie 名。
const csrfCookieName = "harness_csrf";

type AuthStatus = "loading" | "authenticated" | "anonymous";

interface AuthContextValue {
  // user 是当前登录用户，未登录为 null。
  user: User | null;
  // status 表示会话恢复状态。
  status: AuthStatus;
  // login 使用用户名密码登录。
  login: (username: string, password: string) => Promise<void>;
  // register 注册并自动登录。
  register: (username: string, password: string) => Promise<void>;
  // logout 注销当前设备。
  logout: () => Promise<void>;
  // logoutAll 注销全部设备并提升凭据版本。
  logoutAll: () => Promise<void>;
}

const AuthContext = createContext<AuthContextValue | null>(null);

// AuthProvider 负责会话恢复、单飞刷新与注销。
export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null>(null);
  const [status, setStatus] = useState<AuthStatus>("loading");
  const accessTokenRef = useRef<string | null>(null);
  const csrfRef = useRef<string | null>(null);
  const refreshPromiseRef = useRef<Promise<boolean> | null>(null);
  const mountedRef = useRef(true);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  // applySession 写入内存令牌与用户信息。
  const applySession = useCallback((session: AuthSession) => {
    accessTokenRef.current = session.access_token;
    csrfRef.current = session.csrf_token;
    if (mountedRef.current) {
      setUser(session.user);
      setStatus("authenticated");
    }
  }, []);

  // clearSession 清理内存状态；刷新令牌由后端 Cookie 清除。
  const clearSession = useCallback(() => {
    accessTokenRef.current = null;
    csrfRef.current = null;
    if (mountedRef.current) {
      setUser(null);
      setStatus("anonymous");
    }
  }, []);

  // refreshSession 单飞刷新：并发 401 只触发一次轮换，避免令牌重用误判。
  const refreshSession = useCallback(async (): Promise<boolean> => {
    if (refreshPromiseRef.current) {
      return refreshPromiseRef.current;
    }
    const task = (async () => {
      const csrf = csrfRef.current ?? readCookie(csrfCookieName);
      if (!csrf) {
        return false;
      }
      try {
        const response = await fetch("/api/v1/auth/refresh", {
          method: "POST",
          credentials: "same-origin",
          headers: { "X-CSRF-Token": csrf },
        });
        if (!response.ok) {
          return false;
        }
        const session = (await response.json()) as AuthSession;
        applySession(session);
        return true;
      } catch {
        return false;
      }
    })().finally(() => {
      refreshPromiseRef.current = null;
    });
    refreshPromiseRef.current = task;
    return task;
  }, [applySession]);

  useEffect(() => {
    configureApi({
      accessToken: () => accessTokenRef.current,
      csrfToken: () => csrfRef.current ?? readCookie(csrfCookieName),
      refreshSession,
      onUnauthorized: clearSession,
    });
  }, [clearSession, refreshSession]);

  // bootstrap 在页面加载时尝试用 HttpOnly Cookie 恢复会话。
  useEffect(() => {
    let cancelled = false;
    const bootstrap = async () => {
      let csrf = readCookie(csrfCookieName);
      if (!csrf) {
        try {
          const response = await fetch("/api/v1/auth/csrf", { credentials: "same-origin" });
          if (response.ok) {
            const body = (await response.json()) as { csrf_token?: string };
            csrf = body.csrf_token ?? null;
          }
        } catch {
          csrf = null;
        }
      }
      if (!csrf || cancelled) {
        if (!cancelled) {
          setStatus("anonymous");
        }
        return;
      }
      try {
        const response = await fetch("/api/v1/auth/refresh", {
          method: "POST",
          credentials: "same-origin",
          headers: { "X-CSRF-Token": csrf },
        });
        if (!response.ok || cancelled) {
          if (!cancelled) {
            setStatus("anonymous");
          }
          return;
        }
        const session = (await response.json()) as AuthSession;
        if (!cancelled) {
          applySession(session);
        }
      } catch {
        if (!cancelled) {
          setStatus("anonymous");
        }
      }
    };
    void bootstrap();
    return () => {
      cancelled = true;
    };
  }, [applySession]);

  const login = useCallback(
    async (username: string, password: string) => {
      const session = await requestJSON<AuthSession>("/api/v1/auth/login", {
        method: "POST",
        body: JSON.stringify({ username, password }),
      });
      applySession(session);
    },
    [applySession],
  );

  const register = useCallback(
    async (username: string, password: string) => {
      const session = await requestJSON<AuthSession>("/api/v1/auth/register", {
        method: "POST",
        body: JSON.stringify({ username, password }),
      });
      applySession(session);
    },
    [applySession],
  );

  const logout = useCallback(async () => {
    const csrf = csrfRef.current ?? readCookie(csrfCookieName);
    try {
      await fetch("/api/v1/auth/logout", {
        method: "POST",
        credentials: "same-origin",
        headers: csrf ? { "X-CSRF-Token": csrf } : undefined,
      });
    } catch {
      // 本地清理优先于网络结果。
    }
    clearSession();
  }, [clearSession]);

  const logoutAll = useCallback(async () => {
    try {
      await requestJSON<void>("/api/v1/auth/logout-all", { method: "POST" });
    } catch (error) {
      if (!(error instanceof ApiError) || error.status !== 401) {
        throw error;
      }
    }
    clearSession();
  }, [clearSession]);

  const value = useMemo<AuthContextValue>(
    () => ({ user, status, login, register, logout, logoutAll }),
    [user, status, login, register, logout, logoutAll],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

// useAuth 读取鉴权上下文；缺失时抛出明确错误。
export function useAuth(): AuthContextValue {
  const context = useContext(AuthContext);
  if (!context) {
    throw new Error("useAuth must be used inside AuthProvider");
  }
  return context;
}
