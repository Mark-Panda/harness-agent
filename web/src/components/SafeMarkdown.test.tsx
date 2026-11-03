// Markdown 安全渲染测试：原始 HTML、危险协议链接与图片均不得直接执行或加载。
import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { SafeMarkdown } from "./SafeMarkdown";

describe("SafeMarkdown", () => {
  it("skips raw HTML and dangerous link protocols", () => {
    const { container } = render(
      <SafeMarkdown content={'<script>alert("xss")</script>\n\n[恶意链接](javascript:alert(1))\n\n**安全粗体**'} />,
    );
    expect(container.querySelector("script")).toBeNull();
    expect(container.querySelector('a[href^="javascript"]')).toBeNull();
    expect(screen.getByText("安全粗体")).toBeInTheDocument();
  });

  it("renders external links with noopener noreferrer", () => {
    const { container } = render(<SafeMarkdown content={"[文档](https://example.com/docs)"} />);
    const anchor = container.querySelector("a");
    expect(anchor?.getAttribute("href")).toBe("https://example.com/docs");
    expect(anchor?.getAttribute("rel")).toBe("noopener noreferrer");
    expect(anchor?.getAttribute("target")).toBe("_blank");
  });

  it("renders images as links instead of inline loads", () => {
    const { container } = render(<SafeMarkdown content={"![alt](https://example.com/a.png)"} />);
    expect(container.querySelector("img")).toBeNull();
    expect(screen.getByText(/查看图片/)).toBeInTheDocument();
  });
});
