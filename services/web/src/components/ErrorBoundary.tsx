import { Component, type ErrorInfo, type ReactNode } from "react";

/** Keeps a render error in one page (or one message) from blanking the whole app. */
export default class ErrorBoundary extends Component<{ children: ReactNode; fallback?: ReactNode }, { error: Error | null }> {
  state: { error: Error | null } = { error: null };

  static getDerivedStateFromError(error: Error) {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error("render error", error, info.componentStack);
  }

  render() {
    if (!this.state.error) return this.props.children;
    if (this.props.fallback !== undefined) return this.props.fallback;
    return (
      <div className="card p-6 text-sm text-ink-2" role="alert">
        <div className="font-medium text-ink-1">Something went wrong on this page.</div>
        <div className="mono mt-1 break-words text-xs text-ink-3">{this.state.error.message}</div>
        <button className="btn-outline mt-3" onClick={() => this.setState({ error: null })}>Try again</button>
      </div>
    );
  }
}
