import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from "react";

type Mode = "light" | "dark" | "system";
interface ThemeState {
  mode: Mode;
  resolved: "light" | "dark";
  setMode: (m: Mode) => void;
  colors: ChartColors;
}

export interface ChartColors {
  series: string[];
  grid: string;
  axis: string;
  text1: string;
  text2: string;
  text3: string;
  surface1: string;
  surface2: string;
  accent: string;
  seq: string[];
  status: { good: string; warning: string; serious: string; critical: string };
  neutral: string;
}

const Ctx = createContext<ThemeState | null>(null);

function readColors(): ChartColors {
  const s = getComputedStyle(document.documentElement);
  const v = (n: string) => s.getPropertyValue(n).trim();
  return {
    series: [1, 2, 3, 4, 5, 6, 7, 8].map((i) => v(`--series-${i}`)),
    grid: v("--grid"),
    axis: v("--axis"),
    text1: v("--text-primary"),
    text2: v("--text-secondary"),
    text3: v("--text-muted"),
    surface1: v("--surface-1"),
    surface2: v("--surface-2"),
    accent: v("--accent"),
    seq: [v("--seq-100"), v("--seq-300"), v("--seq-500"), v("--seq-700")],
    status: { good: v("--status-good"), warning: v("--status-warning"), serious: v("--status-serious"), critical: v("--status-critical") },
    neutral: v("--neutral-mark"),
  };
}

function safeGet(k: string) {
  try {
    return localStorage.getItem(k);
  } catch {
    return null;
  }
}

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [mode, setModeState] = useState<Mode>((safeGet("fp.theme") as Mode) || "system");
  const [osDark, setOsDark] = useState(() => window.matchMedia?.("(prefers-color-scheme: dark)").matches ?? false);
  const [colors, setColors] = useState<ChartColors>(() => readColors());

  useEffect(() => {
    const mq = window.matchMedia?.("(prefers-color-scheme: dark)");
    const fn = (e: MediaQueryListEvent) => setOsDark(e.matches);
    mq?.addEventListener("change", fn);
    return () => mq?.removeEventListener("change", fn);
  }, []);

  const resolved: "light" | "dark" = mode === "system" ? (osDark ? "dark" : "light") : mode;

  useEffect(() => {
    const el = document.documentElement;
    if (mode === "system") el.removeAttribute("data-theme");
    else el.setAttribute("data-theme", mode);
    // Charts need concrete colours (SVG attributes cannot resolve var()).
    requestAnimationFrame(() => setColors(readColors()));
  }, [mode, osDark]);

  const value = useMemo<ThemeState>(
    () => ({
      mode,
      resolved,
      colors,
      setMode: (m) => {
        setModeState(m);
        try {
          localStorage.setItem("fp.theme", m);
        } catch {
          /* storage unavailable */
        }
      },
    }),
    [mode, resolved, colors],
  );
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useTheme() {
  const c = useContext(Ctx);
  if (!c) throw new Error("useTheme outside provider");
  return c;
}
