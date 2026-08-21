import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";

import { FixtureStepper } from "@/components/dashboard/fixture-stepper";
import { ApiError, getRunFixture, type RunFixture } from "@/lib/api";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, getRunFixture: vi.fn() };
});

const mockGet = vi.mocked(getRunFixture);

afterEach(() => {
  mockGet.mockReset();
});

const RECORDED: RunFixture = {
  runId: "r1",
  agent: "team/assistant",
  recorded: true,
  steps: [
    {
      kind: "model",
      recorded: true,
      request: { model: "gpt", messages: [] },
      response: "data: hello\n\n",
      contentType: "text/event-stream",
      statusCode: 200,
    },
    {
      kind: "tool",
      toolName: "delegate_to",
      recorded: false,
      gapReason: "not captured (a launcher-plane/synthetic tool, or a dropped capture)",
    },
  ],
};

describe("FixtureStepper", () => {
  it("renders a recorded run's steps and reveals byte-exact I/O on expand", async () => {
    mockGet.mockResolvedValue(RECORDED);
    render(<FixtureStepper runId="r1" />);

    await waitFor(() => expect(screen.getByTestId("fixture-stepper")).toBeInTheDocument());
    expect(screen.getByTestId("fixture-step-row-0")).toHaveTextContent("Step 1 · model");
    expect(screen.getByTestId("fixture-step-row-1")).toHaveTextContent("delegate_to");

    // The gapped tool step is badged, never given fabricated I/O.
    expect(screen.getByTestId("fixture-step-gap-1")).toBeInTheDocument();

    // Expanding the model step reveals the verbatim recorded response bytes (incl. SSE framing).
    fireEvent.click(screen.getByTestId("fixture-step-row-0"));
    const io = await screen.findByTestId("fixture-step-io-0");
    expect(io).toHaveTextContent("data: hello");
    expect(io).toHaveTextContent("text/event-stream");
  });

  it("shows an honest note for a not-recorded run (never fabricated I/O)", async () => {
    mockGet.mockResolvedValue({ ...RECORDED, recorded: false });
    render(<FixtureStepper runId="r1" />);
    await waitFor(() => expect(screen.getByTestId("fixture-stepper-note")).toHaveTextContent("was not recorded"));
    expect(screen.queryByTestId("fixture-stepper")).not.toBeInTheDocument();
  });

  it("shows a not-configured note on a 501 (no object store)", async () => {
    mockGet.mockRejectedValue(new ApiError("no object store", 501));
    render(<FixtureStepper runId="r1" />);
    await waitFor(() => expect(screen.getByTestId("fixture-stepper-note")).toHaveTextContent("not configured"));
  });

  it("renders nothing when access is denied (403/404 — no oracle)", async () => {
    mockGet.mockRejectedValue(new ApiError("forbidden", 403));
    const { container } = render(<FixtureStepper runId="r1" />);
    await waitFor(() => expect(mockGet).toHaveBeenCalled());
    expect(screen.queryByTestId("fixture-stepper")).not.toBeInTheDocument();
    expect(screen.queryByTestId("fixture-stepper-note")).not.toBeInTheDocument();
    expect(container).toBeEmptyDOMElement();
  });
});
