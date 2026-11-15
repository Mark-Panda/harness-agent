import { act, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";
import { Card, Field, Input, Textarea } from "./ui";

type RevealResult = { configured: boolean; value?: string };

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}

describe("Field", () => {
  it("links its hint and shows a field-level required error until the value changes", () => {
    render(<Field label="服务名称" hint="填写一个易于识别的名称"><Input required /></Field>);
    const input = screen.getByRole("textbox", { name: "服务名称" });

    expect(input).toHaveAccessibleDescription("填写一个易于识别的名称");
    fireEvent.invalid(input);
    expect(input).toHaveAttribute("aria-invalid", "true");
    expect(input).toHaveAccessibleDescription("请填写此项。");

    fireEvent.change(input, { target: { value: "模型服务" } });
    expect(input).not.toHaveAttribute("aria-invalid", "true");
    expect(input).toHaveAccessibleDescription("填写一个易于识别的名称");
  });

  it("keeps a collapsed section closed until the controlled open state changes", () => {
    function Harness() {
      const [open, setOpen] = useState(false);
      return (
        <>
          <button type="button" onClick={() => setOpen(true)}>展开输入输出</button>
          <Card title="输入与输出" collapsible open={open} onOpenChange={setOpen}>
            <p>载荷内容</p>
          </Card>
        </>
      );
    }
    render(<Harness />);
    expect(screen.queryByText("载荷内容")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "展开输入输出" }));
    expect(screen.getByText("载荷内容")).toBeInTheDocument();
  });

  it("reveals a password without submitting the form or changing the value", () => {
    render(<form onSubmit={(event) => { throw new Error(`Unexpected form submission: ${event.type}`); }}>
      <Field label="访问密钥"><Input type="password" defaultValue="example-draft" /></Field>
    </form>);
    const input = screen.getByLabelText("访问密钥");
    expect(input).toHaveAttribute("type", "password");

    fireEvent.click(screen.getByRole("button", { name: "显示密码或密钥" }));
    expect(input).toHaveAttribute("type", "text");
    expect(input).toHaveValue("example-draft");

    fireEvent.click(screen.getByRole("button", { name: "隐藏密码或密钥" }));
    expect(input).toHaveAttribute("type", "password");
  });
});

describe("saved credential reveal", () => {
  it("shows a saved value read-only without changing the draft, then restores the empty draft on hide", async () => {
    const onRevealSaved = vi.fn().mockResolvedValue({ configured: true, value: "saved-test-secret" });
    const onChange = vi.fn();
    function Harness() {
      const [draft, setDraft] = useState("");
      return (
        <>
          <output aria-label="凭据草稿">{draft}</output>
          <span>{draft ? "已修改" : "未修改"}</span>
          <Field label="访问密钥">
            <Input type="password" value={draft} onChange={(event) => { onChange(event); setDraft(event.target.value); }} onRevealSaved={onRevealSaved} />
          </Field>
        </>
      );
    }
    render(<Harness />);
    const input = screen.getByLabelText("访问密钥");

    fireEvent.click(screen.getByRole("button", { name: "显示密码或密钥" }));
    expect(await screen.findByDisplayValue("saved-test-secret")).toBe(input);
    expect(input).toHaveAttribute("type", "text");
    expect(input).toHaveAttribute("readonly");
    expect(screen.getByLabelText("凭据草稿")).toBeEmptyDOMElement();
    expect(screen.getByText("未修改")).toBeInTheDocument();
    expect(onChange).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "隐藏密码或密钥" }));
    expect(input).toHaveValue("");
    expect(input).toHaveAttribute("type", "password");
    expect(input).not.toHaveAttribute("readonly");
    expect(onRevealSaved).toHaveBeenCalledTimes(1);
    expect(onChange).not.toHaveBeenCalled();
  });

  it("shows a retryable error and can reveal successfully on retry", async () => {
    const onRevealSaved = vi.fn()
      .mockRejectedValueOnce(new Error("temporary failure"))
      .mockResolvedValueOnce({ configured: true, value: "retry-test-secret" });
    render(<Field label="访问密钥"><Input type="password" value="" onChange={() => undefined} onRevealSaved={onRevealSaved} /></Field>);

    fireEvent.click(screen.getByRole("button", { name: "显示密码或密钥" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("重试");
    expect(screen.getByLabelText("访问密钥")).toHaveValue("");

    fireEvent.click(screen.getByRole("button", { name: "显示密码或密钥" }));
    expect(await screen.findByDisplayValue("retry-test-secret")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(onRevealSaved).toHaveBeenCalledTimes(2);
  });

  it("prevents duplicate requests while a reveal is pending", async () => {
    const pending = deferred<RevealResult>();
    const onRevealSaved = vi.fn().mockReturnValue(pending.promise);
    render(<Field label="访问密钥"><Input type="password" value="" onChange={() => undefined} onRevealSaved={onRevealSaved} /></Field>);
    const button = screen.getByRole("button", { name: "显示密码或密钥" });

    fireEvent.click(button);
    fireEvent.click(button);
    expect(onRevealSaved).toHaveBeenCalledTimes(1);
    expect(button).toBeDisabled();
    expect(button).toHaveAttribute("aria-busy", "true");

    await act(async () => {
      pending.resolve({ configured: true, value: "single-request-secret" });
      await pending.promise;
    });
    expect(await screen.findByDisplayValue("single-request-secret")).toBeInTheDocument();
  });

  it("shows 未配置 for an unconfigured saved credential", async () => {
    const onRevealSaved = vi.fn().mockResolvedValue({ configured: false });
    render(<Field label="访问密钥"><Input type="password" value="" onChange={() => undefined} onRevealSaved={onRevealSaved} /></Field>);

    fireEvent.click(screen.getByRole("button", { name: "显示密码或密钥" }));
    const input = await screen.findByDisplayValue("未配置");
    expect(input).toHaveAttribute("readonly");
    expect(onRevealSaved).toHaveBeenCalledTimes(1);
  });

  it("only toggles a non-empty draft locally without requesting the saved credential", () => {
    const onRevealSaved = vi.fn();
    render(<Field label="访问密钥"><Input type="password" value="local-draft-secret" onChange={() => undefined} onRevealSaved={onRevealSaved} /></Field>);
    const input = screen.getByLabelText("访问密钥");

    fireEvent.click(screen.getByRole("button", { name: "显示密码或密钥" }));
    expect(input).toHaveAttribute("type", "text");
    expect(input).toHaveValue("local-draft-secret");
    expect(onRevealSaved).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "隐藏密码或密钥" }));
    expect(input).toHaveAttribute("type", "password");
    expect(onRevealSaved).not.toHaveBeenCalled();
  });

  it("ignores a pending reveal that resolves after unmount", async () => {
    const pending = deferred<RevealResult>();
    const onRevealSaved = vi.fn().mockReturnValue(pending.promise);
    const { unmount } = render(<Field label="访问密钥"><Input type="password" value="" onChange={() => undefined} onRevealSaved={onRevealSaved} /></Field>);
    fireEvent.click(screen.getByRole("button", { name: "显示密码或密钥" }));
    unmount();

    await act(async () => {
      pending.resolve({ configured: true, value: "late-test-secret" });
      await pending.promise;
    });
    expect(screen.queryByDisplayValue("late-test-secret")).toBeNull();
    expect(onRevealSaved).toHaveBeenCalledTimes(1);
  });

  it("ignores a pending reveal after the saved resource changes", async () => {
    const pending = deferred<RevealResult>();
    const firstReveal = vi.fn().mockReturnValue(pending.promise);
    const secondReveal = vi.fn().mockResolvedValue({ configured: true, value: "second-resource-secret" });
    const view = (onRevealSaved: () => Promise<RevealResult>) => (
      <Field label="访问密钥"><Input type="password" value="" onChange={() => undefined} onRevealSaved={onRevealSaved} /></Field>
    );
    const { rerender } = render(view(firstReveal));
    fireEvent.click(screen.getByRole("button", { name: "显示密码或密钥" }));
    rerender(view(secondReveal));

    await act(async () => {
      pending.resolve({ configured: true, value: "first-resource-secret" });
      await pending.promise;
    });
    expect(screen.queryByDisplayValue("first-resource-secret")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "显示密码或密钥" }));
    expect(await screen.findByDisplayValue("second-resource-secret")).toBeInTheDocument();
    expect(firstReveal).toHaveBeenCalledTimes(1);
    expect(secondReveal).toHaveBeenCalledTimes(1);
  });

  it("masks a non-empty Textarea draft until explicitly shown", () => {
    const draft = '{"X-Test":"local-textarea-secret"}';
    const onRevealSaved = vi.fn();
    render(<Field label="请求头 JSON"><Textarea value={draft} onChange={() => undefined} onRevealSaved={onRevealSaved} /></Field>);
    const textarea = screen.getByLabelText("请求头 JSON");

    expect(textarea).toHaveValue("••••••••");
    expect(textarea).toHaveAttribute("readonly");
    fireEvent.click(screen.getByRole("button", { name: "显示密码或密钥" }));
    expect(textarea).toHaveValue(draft);
    expect(textarea).not.toHaveAttribute("readonly");
    expect(onRevealSaved).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "隐藏密码或密钥" }));
    expect(textarea).toHaveValue("••••••••");
    expect(textarea).toHaveAttribute("readonly");
    expect(onRevealSaved).not.toHaveBeenCalled();
  });

  it("reveals and hides a saved Textarea value without writing the draft", async () => {
    const savedHeaders = '{"X-Test":"saved-textarea-secret"}';
    const onRevealSaved = vi.fn().mockResolvedValue({ configured: true, value: savedHeaders });
    const onChange = vi.fn();
    function Harness() {
      const [draft, setDraft] = useState("");
      return (
        <>
          <output aria-label="请求头草稿">{draft}</output>
          <Field label="请求头 JSON">
            <Textarea value={draft} onChange={(event) => { onChange(event); setDraft(event.target.value); }} onRevealSaved={onRevealSaved} />
          </Field>
        </>
      );
    }
    render(<Harness />);
    const textarea = screen.getByLabelText("请求头 JSON");

    fireEvent.click(screen.getByRole("button", { name: "显示密码或密钥" }));
    expect(await screen.findByDisplayValue(savedHeaders)).toBe(textarea);
    expect(textarea).toHaveAttribute("readonly");
    expect(screen.getByLabelText("请求头草稿")).toBeEmptyDOMElement();
    expect(onChange).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "隐藏密码或密钥" }));
    expect(textarea).toHaveValue("");
    expect(textarea).not.toHaveAttribute("readonly");
    expect(onRevealSaved).toHaveBeenCalledTimes(1);
    expect(onChange).not.toHaveBeenCalled();
  });
});
