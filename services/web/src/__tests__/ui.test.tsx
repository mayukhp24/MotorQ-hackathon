import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { Kpi, RiskBar, Segmented, SeverityBadge, VehicleStatus } from "../components/ui";

describe("status components never rely on colour alone", () => {
  it("severity badge carries a text label", () => {
    render(<SeverityBadge severity="CRITICAL" />);
    expect(screen.getByText("Critical")).toBeInTheDocument();
  });

  it("unknown severity falls back to Low", () => {
    render(<SeverityBadge severity="WEIRD" />);
    expect(screen.getByText("Low")).toBeInTheDocument();
  });

  it("vehicle status shows a label and an accessible critical marker", () => {
    render(<VehicleStatus status="IDLING" critical />);
    expect(screen.getByText("Idling")).toBeInTheDocument();
    expect(screen.getByLabelText("active critical fault")).toBeInTheDocument();
  });

  it("risk bar shows the percentage and explains it on hover", () => {
    const { container } = render(<RiskBar risk={0.42} />);
    expect(screen.getByText("42%")).toBeInTheDocument();
    expect(container.firstElementChild).toHaveAttribute("title", "42.0% probability of breakdown within 7 days");
  });
});

describe("controls", () => {
  it("segmented control exposes tabs and reports selection", () => {
    const onChange = vi.fn();
    render(<Segmented value="24h" options={[{ value: "24h", label: "24 h" }, { value: "7d", label: "7 days" }]} onChange={onChange} />);
    expect(screen.getByRole("tab", { name: "24 h" })).toHaveAttribute("aria-selected", "true");
    fireEvent.click(screen.getByRole("tab", { name: "7 days" }));
    expect(onChange).toHaveBeenCalledWith("7d");
  });

  it("KPI tile renders label, value and hint", () => {
    render(<Kpi label="Vehicles online" value="98,412" hint="of 100,000" />);
    expect(screen.getByText("Vehicles online")).toBeInTheDocument();
    expect(screen.getByText("98,412")).toBeInTheDocument();
    expect(screen.getByText("of 100,000")).toBeInTheDocument();
  });
});
