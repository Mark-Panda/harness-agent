import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Field, Input } from "./ui";

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
