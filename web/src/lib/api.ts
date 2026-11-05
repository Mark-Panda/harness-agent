// HTTP 客户端：访问令牌只保存在内存，刷新令牌由 HttpOnly Cookie 承担。
import type { RunEvent } from "./types";

// ApiError 表示后端稳定错误码与 HTTP 状态。
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;

  constructor(status: number, code: string) {
    super(code);
    this.status = status;
    this.code = code;
  }
}

// ApiBindings 由鉴权上下文注入，避免模块级循环依赖。
export interface ApiBindings {
  // accessToken 返回当前内存中的访问令牌。
  accessToken: () => string | null;
  // csrfToken 返回当前 CSRF 令牌。
  csrfToken: () => string | null;
  // refreshSession 执行一次单飞刷新，成功返回 true。
  refreshSession: () => Promise<boolean>;
  // onUnauthorized 在刷新失败时通知鉴权上下文清理状态。
  onUnauthorized: () => void;
}

let bindings: ApiBindings = {
  accessToken: () => null,
  csrfToken: () => null,
  refreshSession: async () => false,
  onUnauthorized: () => undefined,
};

// configureApi 注册鉴权上下文提供的令牌与刷新能力。
export function configureApi(next: ApiBindings) {
  bindings = next;
}

// readCookie 读取同源 Cookie 值，用于恢复 CSRF 令牌。
export function readCookie(name: string): string | null {
  const prefix = `${name}=`;
  for (const part of document.cookie.split(";")) {
    const item = part.trim();
    if (item.startsWith(prefix)) {
      return decodeURIComponent(item.slice(prefix.length));
    }
  }
  return null;
}

// requestJSON 发起 JSON 请求；遇到 401 时单飞刷新并重试一次。
export async function requestJSON<T>(path: string, init: RequestInit = {}, retry = true): Promise<T> {
  const method = (init.method ?? "GET").toUpperCase();
  const headers = new Headers(init.headers);
  headers.set("Accept", "application/json");
  if (init.body !== undefined && init.body !== null && !(init.body instanceof FormData)) {
    headers.set("Content-Type", "application/json");
  }
  const token = bindings.accessToken();
  if (token) {
    headers.set("Authorization", `Bearer ${token}`);
  }
  if (method !== "GET" && method !== "HEAD") {
    const csrf = bindings.csrfToken();
    if (csrf) {
      headers.set("X-CSRF-Token", csrf);
    }
  }
  const response = await fetch(path, { ...init, method, headers, credentials: "same-origin" });
  if (response.status === 401 && retry && !path.startsWith("/api/v1/auth/")) {
    const refreshed = await bindings.refreshSession();
    if (refreshed) {
      return requestJSON<T>(path, init, false);
    }
    bindings.onUnauthorized();
    throw new ApiError(401, "unauthorized");
  }
  if (!response.ok) {
    throw await parseError(response);
  }
  if (response.status === 204) {
    return undefined as T;
  }
  const contentType = response.headers.get("Content-Type") ?? "";
  if (contentType.includes("application/json")) {
    return (await response.json()) as T;
  }
  return (await response.text()) as T;
}

// parseError 解析后端统一错误信封，缺失时回退到状态码描述。
async function parseError(response: Response): Promise<ApiError> {
  try {
    const body = (await response.json()) as { error?: { code?: string } };
    if (body?.error?.code) {
      return new ApiError(response.status, body.error.code);
    }
  } catch {
    // 非 JSON 错误体由状态码兜底。
  }
  return new ApiError(response.status, `http_${response.status}`);
}

// mutate 是写请求的便捷封装，并附带幂等键避免重复提交。
export function mutate<T>(path: string, method: string, body?: unknown, idempotencyKey?: string): Promise<T> {
  const headers = new Headers();
  if (idempotencyKey) {
    headers.set("Idempotency-Key", idempotencyKey);
  }
  return requestJSON<T>(path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
}

// uploadAttachment 以 multipart 上传单个文件，鉴权与 CSRF 由 requestJSON 统一处理。
export function uploadAttachment<T>(path: string, file: File): Promise<T> {
  const body = new FormData();
  body.append("file", file);
  return requestJSON<T>(path, { method: "POST", body });
}

// downloadAttachment 鉴权下载附件并触发浏览器保存，避免把令牌拼进 URL。
export async function downloadAttachment(path: string, filename: string): Promise<void> {
  const attempt = async (retry: boolean): Promise<Response> => {
    const headers = new Headers({ Accept: "*/*" });
    const token = bindings.accessToken();
    if (token) {
      headers.set("Authorization", `Bearer ${token}`);
    }
    const response = await fetch(path, { headers, credentials: "same-origin" });
    if (response.status === 401 && retry) {
      const refreshed = await bindings.refreshSession();
      if (refreshed) {
        return attempt(false);
      }
      bindings.onUnauthorized();
      throw new ApiError(401, "unauthorized");
    }
    return response;
  };
  const response = await attempt(true);
  if (!response.ok) {
    throw await parseError(response);
  }
  const blob = await response.blob();
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = filename;
  document.body.append(anchor);
  anchor.click();
  anchor.remove();
  URL.revokeObjectURL(url);
}

// downloadJSON 通过鉴权请求下载导出文件，禁止把令牌放入 URL。
export async function downloadJSON(path: string, filename: string): Promise<void> {
  const attempt = async (retry: boolean): Promise<Response> => {
    const headers = new Headers({ Accept: "application/json" });
    const token = bindings.accessToken();
    if (token) {
      headers.set("Authorization", `Bearer ${token}`);
    }
    const response = await fetch(path, { headers, credentials: "same-origin" });
    if (response.status === 401 && retry) {
      const refreshed = await bindings.refreshSession();
      if (refreshed) {
        return attempt(false);
      }
      bindings.onUnauthorized();
      throw new ApiError(401, "unauthorized");
    }
    return response;
  };
  const response = await attempt(true);
  if (!response.ok) {
    throw await parseError(response);
  }
  const blob = await response.blob();
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = filename;
  document.body.append(anchor);
  anchor.click();
  anchor.remove();
  URL.revokeObjectURL(url);
}

// streamRunEvents 通过 fetch 流式读取 SSE，Authorization 不进入 URL。
export async function streamRunEvents(
  runId: string,
  options: {
    signal?: AbortSignal;
    lastEventId?: number;
    onEvent: (event: RunEvent) => void;
    onOpen?: () => void;
  },
): Promise<void> {
  const headers = new Headers({ Accept: "text/event-stream" });
  const token = bindings.accessToken();
  if (token) {
    headers.set("Authorization", `Bearer ${token}`);
  }
  if (options.lastEventId && options.lastEventId > 0) {
    headers.set("Last-Event-ID", String(options.lastEventId));
  }
  let response = await fetch(`/api/v1/runs/${encodeURIComponent(runId)}/events`, {
    headers,
    credentials: "same-origin",
    signal: options.signal,
  });
  if (response.status === 401) {
    const refreshed = await bindings.refreshSession();
    if (!refreshed) {
      bindings.onUnauthorized();
      throw new ApiError(401, "unauthorized");
    }
    const retryHeaders = new Headers(headers);
    const newToken = bindings.accessToken();
    if (newToken) {
      retryHeaders.set("Authorization", `Bearer ${newToken}`);
    }
    response = await fetch(`/api/v1/runs/${encodeURIComponent(runId)}/events`, {
      headers: retryHeaders,
      credentials: "same-origin",
      signal: options.signal,
    });
  }
  if (!response.ok) {
    throw await parseError(response);
  }
  if (!response.body) {
    throw new ApiError(502, "stream_unavailable");
  }
  options.onOpen?.();
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let eventName = "message";
  let dataLines: string[] = [];
  let eventId = 0;

  // dispatch 派发完整的 SSE 帧；心跳不产生 Run 事件。
  const dispatch = () => {
    if (dataLines.length === 0) {
      eventName = "message";
      return;
    }
    const raw = dataLines.join("\n");
    dataLines = [];
    if (eventName === "heartbeat") {
      eventName = "message";
      return;
    }
    let parsed: Record<string, unknown> = {};
    try {
      parsed = JSON.parse(raw) as Record<string, unknown>;
    } catch {
      parsed = { raw };
    }
    options.onEvent({ seq: eventId, event: eventName, data: parsed });
    eventName = "message";
  };

  while (true) {
    const { done, value } = await reader.read();
    if (done) {
      break;
    }
    buffer += decoder.decode(value, { stream: true });
    const blocks = buffer.split("\n\n");
    buffer = blocks.pop() ?? "";
    for (const block of blocks) {
      for (const line of block.split("\n")) {
        if (line.startsWith(":")) {
          continue;
        }
        if (line.startsWith("event:")) {
          eventName = line.slice(6).trim();
        } else if (line.startsWith("data:")) {
          dataLines.push(line.slice(5).trimStart());
        } else if (line.startsWith("id:")) {
          const parsed = Number.parseInt(line.slice(3).trim(), 10);
          if (!Number.isNaN(parsed) && parsed > 0) {
            eventId = parsed;
          }
        }
      }
      dispatch();
    }
  }
  dispatch();
}
