import { render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import App from "./App";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("App", () => {
  it("shows backend readiness", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: true,
        json: () => Promise.resolve({ status: "ready" }),
      }),
    );
    render(<App />);
    expect(await screen.findByText("后端连接正常")).toBeInTheDocument();
  });

  it("shows an unavailable state when the backend fails", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("offline")));
    render(<App />);
    expect(await screen.findByText("后端暂不可用")).toBeInTheDocument();
  });
});
