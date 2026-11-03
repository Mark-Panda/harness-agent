// API 客户端测试：SSE 解析、鉴权重试与 Cookie 读取。
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, configureApi, readCookie, requestJSON, streamRunEvents } from "./api";

beforeEach(() => {
  configureApi({
    accessToken: () => "token-1",
    csrfToken: () => "csrf",
    refreshSession: async () => false,
    onUnauthorized: () => undefined,
  });
});

afterEach(() => {
  vi.unstubAllGlobals();
  document.cookie = "harness_csrf=; Max-Age=0; path=/";
});

describe("api client", () => {
  it("parses SSE frames by sequence and ignores heartbeats", async () => {
    const encoder = new TextEncoder();
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(encoder.encode("id: 1\nevent: message.created\ndata: {\"content\":\"hi\"}\n\n"));
        controller.enqueue(encoder.encode("event: heartbeat\ndata: {\"status\":\"ok\"}\n\n"));
        controller.enqueue(encoder.encode("id: 2\nevent: run.completed\ndata: {\"total_tokens\":3}\n\n"));
        controller.close();
      },
    });
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, status: 200, body: stream }));
    const events: Array<{ seq: number; event: string; data: Record<string, unknown> }> = [];
    await streamRunEvents("run-1", { onEvent: (event) => events.push(event) });
    expect(events).toEqual([
      { seq: 1, event: "message.created", data: { content: "hi" } },
      { seq: 2, event: "run.completed", data: { total_tokens: 3 } },
    ]);
  });

  it("refreshes once and retries an unauthorized request", async () => {
    let refreshed = 0;
    let accessToken = "expired";
    configureApi({
      accessToken: () => accessToken,
      csrfToken: () => "csrf",
      refreshSession: async () => {
        refreshed += 1;
        accessToken = "fresh";
        return true;
      },
      onUnauthorized: () => undefined,
    });
    const fetchMock = vi.fn().mockImplementation((_input: RequestInfo | URL, init?: RequestInit) => {
      const authorization = new Headers(init?.headers).get("Authorization");
      if (authorization === "Bearer expired") {
        return Promise.resolve({ ok: false, status: 401, json: () => Promise.resolve({ error: { code: "unauthorized" } }) });
      }
      return Promise.resolve({ ok: true, status: 200, headers: new Headers({ "Content-Type": "application/json" }), json: () => Promise.resolve({ id: "run-1" }) });
    });
    vi.stubGlobal("fetch", fetchMock);
    const result = await requestJSON<{ id: string }>("/api/v1/runs/run-1");
    expect(result.id).toBe("run-1");
    expect(refreshed).toBe(1);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("reports stable error codes from the error envelope", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false, status: 403, json: () => Promise.resolve({ error: { code: "quota_exceeded" } }) }));
    await expect(requestJSON("/api/v1/agents")).rejects.toMatchObject({ status: 403, code: "quota_exceeded" } satisfies Partial<ApiError>);
  });

  it("reads the csrf cookie value", () => {
    document.cookie = "harness_csrf=abc123; path=/";
    expect(readCookie("harness_csrf")).toBe("abc123");
    expect(readCookie("missing")).toBeNull();
  });
});
