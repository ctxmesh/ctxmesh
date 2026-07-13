import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";

import { UseAgentPanel } from "@/components/dashboard/use-agent-panel";

// UseAgentPanel (M22/U6) tells the user HOW to call the agent, per execution model.

describe("UseAgentPanel", () => {
  it("serving → shows the endpoint + a curl POST to the agent URL", () => {
    render(
      <UseAgentPanel
        name="billing"
        executionModel="serving"
        url="http://billing.default.example.com"
      />,
    );
    const code = screen.getByTestId("use-agent-serving");
    expect(code).toHaveTextContent("curl -X POST http://billing.default.example.com");
    expect(code).toHaveTextContent("Authorization: Bearer $TOKEN");
  });

  it("eventing → shows the CloudEvent trigger with type = the agent name", () => {
    render(<UseAgentPanel name="ledger" executionModel="eventing" url="" />);
    const code = screen.getByTestId("use-agent-eventing");
    expect(code).toHaveTextContent("Ce-Type: ledger");
    expect(code).toHaveTextContent("-broker");
    // Not the serving curl.
    expect(screen.queryByTestId("use-agent-serving")).toBeNull();
  });

  it("job → describes the one-shot run + where to read results", () => {
    render(<UseAgentPanel name="nightly" executionModel="job" url="" />);
    expect(screen.getByTestId("use-agent-panel")).toHaveTextContent(/one-shot job/i);
    expect(screen.queryByTestId("use-agent-serving")).toBeNull();
    expect(screen.queryByTestId("use-agent-eventing")).toBeNull();
  });
});
