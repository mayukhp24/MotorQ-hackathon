import type { ReactNode } from "react";
import type { TooltipProps } from "recharts";
import { useTheme } from "../lib/theme";

/** Shared chart anatomy: recessive grid/axes, ink-token text. */
export function useAxis() {
  const { colors } = useTheme();
  return {
    grid: { stroke: colors.grid, strokeDasharray: "0", vertical: false },
    x: { stroke: colors.grid, tick: { fill: colors.text3, fontSize: 11 }, tickLine: false, axisLine: { stroke: colors.grid } },
    y: { stroke: colors.grid, tick: { fill: colors.text3, fontSize: 11 }, tickLine: false, axisLine: false, width: 44 },
    colors,
  };
}

type Row = { name?: string | number; value?: number | string; color?: string; dataKey?: string | number; payload?: Record<string, unknown> };

export function ChartTooltip({ active, payload, label, labelFormatter, valueFormatter }: TooltipProps<number, string> & {
  labelFormatter?: (l: unknown) => ReactNode; valueFormatter?: (v: number, name: string) => ReactNode;
}) {
  if (!active || !payload?.length) return null;
  const rows = payload as Row[];
  return (
    <div className="rounded-lg border border-line bg-surface-1 px-3 py-2 text-xs shadow-lg">
      <div className="mb-1 font-medium text-ink-1">{labelFormatter ? labelFormatter(label) : String(label)}</div>
      {rows.map((p) => (
        <div key={String(p.dataKey)} className="flex items-center justify-between gap-4">
          <span className="inline-flex items-center gap-1.5 text-ink-2">
            <span className="h-2 w-2 rounded-sm" style={{ background: p.color }} aria-hidden />
            {p.name}
          </span>
          <span className="tabular-nums text-ink-1">
            {valueFormatter ? valueFormatter(Number(p.value), String(p.name)) : Number(p.value).toLocaleString()}
          </span>
        </div>
      ))}
    </div>
  );
}
