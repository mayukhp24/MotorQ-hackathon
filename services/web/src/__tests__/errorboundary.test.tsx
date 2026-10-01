import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import ErrorBoundary from "../components/ErrorBoundary";

let fail = true;
function Flaky() {
  if (fail) throw new Error("bad answer payload");
  return <p>recovered</p>;
}

describe("a render error never blanks the app", () => {
  it("shows the error with a retry instead of an empty page", () => {
    const quiet = vi.spyOn(console, "error").mockImplementation(() => {});
    render(<ErrorBoundary><Flaky /></ErrorBoundary>);
    expect(screen.getByRole("alert")).toHaveTextContent("bad answer payload");
    fail = false;
    fireEvent.click(screen.getByText("Try again"));
    expect(screen.getByText("recovered")).toBeInTheDocument();
    quiet.mockRestore();
  });

  it("uses the given fallback (plain text for a chat message)", () => {
    const quiet = vi.spyOn(console, "error").mockImplementation(() => {});
    fail = true;
    render(<ErrorBoundary fallback={<p>plain text answer</p>}><Flaky /></ErrorBoundary>);
    expect(screen.getByText("plain text answer")).toBeInTheDocument();
    quiet.mockRestore();
  });
});
