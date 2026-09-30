/** @type {import('tailwindcss').Config} */
export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  darkMode: ["class", '[data-theme="dark"]'],
  theme: {
    extend: {
      fontFamily: { sans: ["Inter", "ui-sans-serif", "system-ui", "sans-serif"], mono: ["JetBrains Mono", "ui-monospace", "monospace"] },
      colors: {
        surface: { 0: "var(--surface-0)", 1: "var(--surface-1)", 2: "var(--surface-2)", 3: "var(--surface-3)" },
        ink: { 1: "var(--text-primary)", 2: "var(--text-secondary)", 3: "var(--text-muted)" },
        line: "var(--border)",
        accent: { DEFAULT: "var(--accent)", soft: "var(--accent-soft)", ink: "var(--accent-ink)" },
        status: { good: "var(--status-good)", warning: "var(--status-warning)", serious: "var(--status-serious)", critical: "var(--status-critical)" },
      },
      boxShadow: { card: "0 1px 2px rgba(0,0,0,.06), 0 1px 1px rgba(0,0,0,.04)" },
    },
  },
  plugins: [],
};
