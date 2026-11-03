import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import App from "./App";

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
    render(<App />);
    expect(await screen.findByRole("heading", { name: "登录" })).toBeInTheDocument();
    // 登录页使用面向用户的说明，而不是内部术语。
    expect(screen.getByText("把重复的事，交给一个懂你的助手。")).toBeInTheDocument();
    expect(screen.getByText("高风险操作先征求你的同意")).toBeInTheDocument();
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
    render(<App />);
    // 账户菜单默认收起，先打开再确认注销入口可用。
    fireEvent.click(await screen.findByRole("button", { name: /alice/ }));
    expect(await screen.findByText("注销当前设备")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /创建与发布/ })).toBeInTheDocument();
  });
});
