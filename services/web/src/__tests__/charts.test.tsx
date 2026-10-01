import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { ChartTooltip } from "../components/charts";

const payload = [{ dataKey: "vehicles", name: "Vehicles", value: 53780, color: "#2a78d6" }];

describe("chart tooltip", () => {
  it("formats the category label", () => {
    render(<ChartTooltip active payload={payload as never} label="0–10%" labelFormatter={(l) => `Risk ${l}`} />);
    expect(screen.getByText("Risk 0–10%")).toBeInTheDocument();
  });

  it("never prints 'undefined' when the label is not known yet", () => {
    const { container } = render(<ChartTooltip active payload={payload as never} labelFormatter={(l) => `Risk ${l}`} />);
    expect(container.textContent).not.toMatch(/undefined/);
    expect(screen.getByText("Vehicles")).toBeInTheDocument();
  });
});
