import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import Copilot from "../pages/Copilot";

vi.mock("../lib/auth", () => ({ useAuth: () => ({ can: () => true }) }));
vi.mock("../api/client", () => ({
  api: vi.fn(async (path: string) => path.startsWith("/copilot/actions") ? { items: [] } : {
    conversation_id: "c1", answer: "Most at risk:\n\n| VIN | Risk |\n|---|---|\n| AURCGD4X4RC053378 | 99% |",
    mode: "llm", warnings: [], latency_ms: 900, tool_calls: [{ tool: "list_at_risk_vehicles", args: {}, error: false, result_preview: "" }],
    proposed_actions: [], usage: { model: "openai/gpt-oss-120b", est_cost_usd: 0 },
  }),
}));

const original = Element.prototype.scrollIntoView;
afterEach(() => { Element.prototype.scrollIntoView = original; });

describe("copilot page", () => {
  it("survives browsers whose scrollIntoView returns a Promise (Chrome/Edge 150+)", async () => {
    Element.prototype.scrollIntoView = vi.fn(() => Promise.resolve()) as unknown as typeof original;
    render(
      <QueryClientProvider client={new QueryClient()}>
        <MemoryRouter><Copilot /></MemoryRouter>
      </QueryClientProvider>,
    );
    fireEvent.click(screen.getByText("Which vehicles are most likely to break down this week?"));
    const table = await screen.findByRole("table");                  // GFM table rendered, page still alive
    expect(table).toHaveTextContent("AURCGD4X4RC053378");
    expect(screen.getByText(/openai\/gpt-oss-120b/)).toBeInTheDocument();
  });
});
