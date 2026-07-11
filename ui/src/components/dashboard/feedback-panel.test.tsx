import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";

import { FeedbackPanel } from "@/components/dashboard/feedback-panel";

// FeedbackPanel — m16.9 tests.
//
// Coverage:
//   • renders scores (numeric + categorical / stringValue).
//   • empty list → teaching empty state ("no feedback recorded").
//   • 501 → calm disabled state (no error alert, distinct "unavailable" banner).
//   • 502 → same calm disabled state as 501.
//   • generic error → error notice.

function stubFeedback(body: unknown, ok = true, status = 200) {
  vi.stubGlobal(
    "fetch",
    vi.fn(() =>
      Promise.resolve({
        ok,
        status,
        json: async () => body,
        text: async () => JSON.stringify(body),
      } as Response),
    ),
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("FeedbackPanel (m16.9)", () => {
  it("renders numeric and categorical scores", async () => {
    stubFeedback({
      scores: [
        { id: "s1", name: "accuracy",    value: 0.92,  comment: "correct", source: "human" },
        { id: "s2", name: "tone",        stringValue: "positive", comment: "", source: "auto" },
        { id: "s3", name: "cost-signal", value: 1,     comment: "", source: "" },
      ],
    });
    render(<FeedbackPanel traceId="t1" />);

    await waitFor(() =>
      expect(screen.getByTestId("feedback-score-s1")).toBeInTheDocument(),
    );
    expect(screen.getByTestId("feedback-score-s1")).toHaveTextContent("accuracy");
    expect(screen.getByTestId("feedback-score-s1")).toHaveTextContent("0.92");
    expect(screen.getByTestId("feedback-score-s1")).toHaveTextContent("correct");
    expect(screen.getByTestId("feedback-score-s1")).toHaveTextContent("human");

    // Categorical (stringValue) renders value column correctly.
    expect(screen.getByTestId("feedback-score-s2")).toHaveTextContent("tone");
    expect(screen.getByTestId("feedback-score-s2")).toHaveTextContent("positive");

    expect(screen.getByTestId("feedback-score-s3")).toBeInTheDocument();
  });

  it("shows the empty state when the scores array is empty", async () => {
    stubFeedback({ scores: [] });
    render(<FeedbackPanel traceId="t1" />);

    await waitFor(() =>
      expect(screen.getByTestId("feedback-panel")).toHaveTextContent("No feedback recorded"),
    );
    // Should NOT show a score row.
    expect(screen.queryByTestId(/^feedback-score-/)).toBeNull();
  });

  it("renders a calm disabled state on 501 — no error alert", async () => {
    stubFeedback({ error: "not implemented" }, false, 501);
    render(<FeedbackPanel traceId="t1" />);

    await waitFor(() =>
      expect(screen.getByTestId("feedback-panel")).toHaveTextContent("unavailable"),
    );
    // No role="alert" error block.
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("surfaces a 502 (Langfuse wired but upstream failed) as an error, NOT a calm banner", async () => {
    // A 502 is a real, likely-transient upstream failure — it must be visible,
    // never silently collapsed into the 501 "not connected" calm state.
    stubFeedback({ error: "bad gateway" }, false, 502);
    render(<FeedbackPanel traceId="t1" />);

    await waitFor(() => expect(screen.getByRole("alert")).toBeInTheDocument());
    expect(screen.getByRole("alert")).toHaveTextContent("Couldn't load feedback scores");
    expect(screen.queryByTestId("feedback-panel")).not.toHaveTextContent("unavailable");
  });

  it("renders an error notice on a non-501/502 failure", async () => {
    stubFeedback({ error: "internal server error" }, false, 500);
    render(<FeedbackPanel traceId="t1" />);

    await waitFor(() =>
      expect(screen.getByRole("alert")).toBeInTheDocument(),
    );
    expect(screen.getByRole("alert")).toHaveTextContent("Couldn't load feedback scores");
  });
});
