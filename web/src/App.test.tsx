import { fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createMemoryRouter } from "react-router-dom";
import App, { appRoutes } from "./App";

beforeEach(() => {
  // 测试使用模拟的 fetch；去掉 JSDOM 信号，避免 Node 的 Request 跨运行时校验失败。
  const NativeRequest = Request;
  vi.stubGlobal("Request", class extends NativeRequest {
    constructor(input: RequestInfo | URL, init?: RequestInit) {
      super(input, { ...init, signal: undefined });
    }
  });
});

afterEach(() => {
  vi.unstubAllGlobals();
  document.cookie = "harness_csrf=; Max-Age=0; path=/";
});

// jsonResponse 构造测试用最小响应对象。
function jsonResponse(body: unknown, status = 200) {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
  };
}

describe("App", () => {
  it("shows the login page when no session can be restored", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: RequestInfo | URL) => {
        const url = String(input);
        if (url.endsWith("/api/v1/auth/csrf")) {
          return Promise.resolve(jsonResponse({ csrf_token: "csrf" }));
        }
        return Promise.resolve(jsonResponse({ error: { code: "unauthorized" } }, 401));
      }),
    );
    render(<App router={createMemoryRouter(appRoutes)} />);
    expect(await screen.findByRole("heading", { name: "登录" })).toBeInTheDocument();
    expect(screen.getByText("智能体工作区")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "注册" })).toHaveAttribute("href", "/register");
  });

  it("renders the workspace after restoring a session", async () => {
    const session = {
      user: { id: "u-1", username: "alice", role: "user", status: "active" },
      access_token: "access",
      token_type: "Bearer",
      access_expires_at: "2026-11-02T12:15:00Z",
      refresh_expires_at: "2026-12-02T12:00:00Z",
      csrf_token: "csrf",
    };
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: RequestInfo | URL) => {
        const url = String(input);
        if (url.endsWith("/api/v1/auth/csrf")) {
          return Promise.resolve(jsonResponse({ csrf_token: "csrf" }));
        }
        if (url.endsWith("/api/v1/auth/refresh")) {
          return Promise.resolve(jsonResponse(session));
        }
        return Promise.resolve(jsonResponse({ items: [], next_cursor: null }));
      }),
    );
    render(<App router={createMemoryRouter(appRoutes)} />);
    // 账户菜单默认收起，先打开再确认注销入口可用。
    fireEvent.click(await screen.findByRole("button", { name: /alice/ }));
    expect(await screen.findByText("注销当前设备")).toBeInTheDocument();
    const nav = screen.getByRole("navigation", { name: "主导航" });
    expect(within(nav).getByRole("link", { name: "助手" })).toHaveAttribute("href", "/agents");
    expect(within(screen.getByRole("complementary")).getByRole("link", { name: "新建对话" })).toHaveAttribute("href", "/sessions?new=1");
  });
});
