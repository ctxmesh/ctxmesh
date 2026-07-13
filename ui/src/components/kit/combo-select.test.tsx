import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";

import { ComboSelect } from "@/components/kit/combo-select";

// ComboSelect replaces the <datalist> trap (M22/U2): a real dropdown that always
// shows EVERY option (not filtered to the typed text), with a "Custom…" escape.

describe("ComboSelect", () => {
  it("renders a real <select> listing ALL options (not filtered to the value)", () => {
    render(
      <ComboSelect
        value="claude-fable-5"
        options={["claude-fable-5", "claude-opus-4-8", "claude-sonnet-5"]}
        onChange={() => {}}
        testId="model"
      />,
    );
    const select = screen.getByTestId("model");
    expect(select.tagName).toBe("SELECT");
    // Every option is present — the whole point vs a datalist that would filter
    // suggestions down to the pre-filled "claude-fable-5".
    expect(screen.getByRole("option", { name: "claude-opus-4-8" })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "claude-sonnet-5" })).toBeInTheDocument();
    expect((select as HTMLSelectElement).value).toBe("claude-fable-5");
  });

  it("fires onChange with the picked option", () => {
    const onChange = vi.fn();
    render(
      <ComboSelect
        value="claude-fable-5"
        options={["claude-fable-5", "claude-opus-4-8"]}
        onChange={onChange}
        testId="model"
      />,
    );
    fireEvent.change(screen.getByTestId("model"), {
      target: { value: "claude-opus-4-8" },
    });
    expect(onChange).toHaveBeenCalledWith("claude-opus-4-8");
  });

  it("picking 'Custom…' switches to a free-text input", () => {
    const onChange = vi.fn();
    render(
      <ComboSelect value="" options={["anthropic"]} onChange={onChange} testId="prov" />,
    );
    fireEvent.change(screen.getByTestId("prov"), { target: { value: "__custom__" } });
    // Now a text input (not a select) is shown for the custom value.
    const input = screen.getByTestId("prov");
    expect(input.tagName).toBe("INPUT");
    fireEvent.change(input, { target: { value: "my-openai" } });
    expect(onChange).toHaveBeenLastCalledWith("my-openai");
  });

  it("starts in custom mode when the value isn't a known option", () => {
    render(
      <ComboSelect
        value="some-custom-binding"
        options={["anthropic", "openai"]}
        onChange={() => {}}
        testId="binding"
      />,
    );
    // A pre-set value outside the option set → free-text input, not a mismatched select.
    expect(screen.getByTestId("binding").tagName).toBe("INPUT");
    expect((screen.getByTestId("binding") as HTMLInputElement).value).toBe(
      "some-custom-binding",
    );
  });

  it("respects allowCustom=false (no Custom… option, no free-text)", () => {
    render(
      <ComboSelect
        value="default"
        options={["default", "prod"]}
        onChange={() => {}}
        allowCustom={false}
        testId="ns"
      />,
    );
    expect(screen.getByTestId("ns").tagName).toBe("SELECT");
    expect(screen.queryByRole("option", { name: "Custom…" })).not.toBeInTheDocument();
  });
});
