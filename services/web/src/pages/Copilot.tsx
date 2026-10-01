import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Bot, Check, Send, ShieldAlert, User, Wrench, X } from "lucide-react";
import { useEffect, useRef, useState, type FormEvent } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { api } from "../api/client";
import ErrorBoundary from "../components/ErrorBoundary";
import { Card, Empty, ErrorNote, PageHeader } from "../components/ui";
import { useAuth } from "../lib/auth";
import { fmtAgo } from "../lib/format";

interface Turn {
  conversation_id: string; answer: string; mode: string; warnings: string[]; latency_ms: number;
  tool_calls: { tool: string; args: Record<string, unknown>; error: boolean; result_preview: string }[];
  proposed_actions: { action_id: string; vin: string; component: string; priority: string }[];
  usage: { input_tokens?: number; output_tokens?: number; est_cost_usd?: number; model?: string };
}
interface Msg { role: "user" | "assistant"; text: string; turn?: Turn }
interface Action { action_id: string; tool: string; arguments: { vin: string; component: string; priority: string; reason: string }; status: string; created_at: string; requested_by: string }

const SUGGESTIONS = [
  "Which vehicles are most likely to break down this week?",
  "How much are we losing to idling, and where?",
  "What does P0217 mean and how do I fix it?",
  "Show open critical alerts",
  "Which drivers need safety coaching?",
];

export default function Copilot() {
  const { can } = useAuth();
  const [msgs, setMsgs] = useState<Msg[]>([]);
  const [input, setInput] = useState("");
  const [conv, setConv] = useState<string | undefined>();
  const end = useRef<HTMLDivElement>(null);
  const qc = useQueryClient();
  const chat = useMutation({
    mutationFn: (message: string) => api<Turn>("/copilot/chat", { method: "POST", json: { message, conversation_id: conv } }),
    onSuccess: (t) => {
      setConv(t.conversation_id);
      setMsgs((m) => [...m, { role: "assistant", text: t.answer, turn: t }]);
      if (t.proposed_actions.length) qc.invalidateQueries({ queryKey: ["actions"] });
    },
  });
  // Block body: scroll methods return a Promise in newer browsers (Chrome/Edge 150+), and React would
  // call a returned value as the effect's cleanup ("is not a function").
  useEffect(() => {
    end.current?.scrollIntoView({ behavior: "smooth", block: "nearest" });
  }, [msgs, chat.isPending]);

  function send(text: string) {
    if (!text.trim() || chat.isPending) return;
    setMsgs((m) => [...m, { role: "user", text }]);
    setInput("");
    chat.mutate(text);
  }

  return (
    <>
      <PageHeader title="Maintenance copilot" subtitle="Ask about your fleet in plain language. Answers come only from your organisation's data; every tool call is audited and actions need approval." />
      <div className="grid gap-4 xl:grid-cols-[1fr_380px]">
        <Card pad={false} className="flex h-[calc(100vh-220px)] min-h-[480px] flex-col" bodyClassName="flex min-h-0 flex-1 flex-col">
          <div className="min-h-0 flex-1 space-y-4 overflow-y-auto p-4 sm:p-5">
            {msgs.length === 0 && (
              <div className="anim-fade-up mx-auto max-w-xl pt-8 text-center">
                <Bot className="mx-auto h-8 w-8 text-accent" />
                <p className="mt-3 text-sm text-ink-2">Try one of these:</p>
                <div className="mt-3 flex flex-wrap justify-center gap-2">
                  {SUGGESTIONS.map((s, i) => (
                    <button key={s} className="btn-outline anim-fade-up text-left hover:border-accent" style={{ animationDelay: `${80 + i * 50}ms` }} onClick={() => send(s)}>{s}</button>
                  ))}
                </div>
              </div>
            )}
            {msgs.map((m, i) => <Bubble key={i} m={m} />)}
            {chat.isPending && (
              <div className="anim-fade-in flex items-center gap-2 text-sm text-ink-3" role="status">
                <Bot className="h-4 w-4" /> Thinking and querying fleet data
                <span className="inline-flex gap-1 pl-0.5" aria-hidden><span className="typing-dot" /><span className="typing-dot" /><span className="typing-dot" /></span>
              </div>
            )}
            {chat.error && <ErrorNote error={chat.error} />}
            <div ref={end} />
          </div>
          <form className="flex gap-2 border-t border-line p-3" onSubmit={(e: FormEvent) => { e.preventDefault(); send(input); }}>
            <input className="input" placeholder="e.g. Why is AURCGD4X... at risk? Schedule a fix for it." value={input} onChange={(e) => setInput(e.target.value)} maxLength={2000} aria-label="Message" />
            <button className="btn-primary" disabled={chat.isPending || !input.trim()}><Send className="h-4 w-4" /> Send</button>
          </form>
        </Card>
        <Approvals canApprove={can("copilot:approve")} />
      </div>
    </>
  );
}

function Bubble({ m }: { m: Msg }) {
  const Icon = m.role === "user" ? User : Bot;
  return (
    <div className={m.role === "user" ? "anim-pop-in flex justify-end" : "anim-pop-in flex"}>
      <div className={m.role === "user" ? "max-w-[80%] rounded-2xl rounded-br-md bg-accent px-4 py-2.5 text-sm text-white" : "max-w-[92%]"}>
        {m.role === "assistant" ? (
          <div className="flex gap-3">
            <div className="grid h-7 w-7 shrink-0 place-items-center rounded-full bg-accent-soft"><Icon className="h-4 w-4 text-accent-ink" /></div>
            <div className="min-w-0">
              <div className="markdown text-sm leading-relaxed text-ink-1">
                <ErrorBoundary fallback={<p className="whitespace-pre-wrap">{String(m.text ?? "")}</p>}>
                  <ReactMarkdown remarkPlugins={[remarkGfm]}>{String(m.text ?? "")}</ReactMarkdown>
                </ErrorBoundary>
              </div>
              {m.turn && (
                <div className="mt-2 flex flex-wrap items-center gap-1.5 text-[11px] text-ink-3">
                  {m.turn.tool_calls.map((t, i) => (
                    <span key={i} className="rounded-md border border-line bg-surface-2 px-1.5 py-0.5 font-mono" title={JSON.stringify(t.args)}>{t.error ? "✕ " : ""}{t.tool}</span>
                  ))}
                  <span>· {m.turn.mode === "llm" ? m.turn.usage.model : m.turn.mode} · {(m.turn.latency_ms / 1000).toFixed(1)} s</span>
                  {m.turn.usage.est_cost_usd != null && <span>· ${m.turn.usage.est_cost_usd.toFixed(4)}</span>}
                </div>
              )}
              {m.turn?.warnings.filter((w) => !w.startsWith("Answered by")).map((w) => (
                <div key={w} className="mt-1.5 inline-flex items-center gap-1 text-xs text-ink-2"><ShieldAlert className="h-3.5 w-3.5" style={{ color: "var(--status-warning)" }} /> {w}</div>
              ))}
            </div>
          </div>
        ) : m.text}
      </div>
    </div>
  );
}

function Approvals({ canApprove }: { canApprove: boolean }) {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["actions"], queryFn: () => api<{ items: Action[] }>("/copilot/actions?status=PROPOSED"), refetchInterval: 15_000 });
  const decide = useMutation({
    mutationFn: ({ id, d }: { id: string; d: "approve" | "reject" }) => api(`/copilot/actions/${id}/${d}`, { method: "POST" }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["actions"] }); qc.invalidateQueries({ queryKey: ["work-orders"] }); },
  });
  return (
    <Card title="Pending approvals" subtitle="Human-in-the-loop: the copilot can only propose" pad={false}>
      {decide.error && <div className="p-3"><ErrorNote error={decide.error} /></div>}
      {(q.data?.items.length ?? 0) === 0 ? <Empty>No proposals waiting. Ask the copilot to schedule a repair for a vehicle.</Empty> : (
        <ul className="divide-y divide-line">
          {q.data!.items.map((a) => (
            <li key={a.action_id} className="anim-fade-up px-4 py-3 sm:px-5">
              <div className="flex items-center gap-2 text-sm font-medium text-ink-1"><Wrench className="h-4 w-4 text-ink-3" /> Work order · {a.arguments.priority}</div>
              <div className="mono mt-1 text-ink-2">{a.arguments.vin}</div>
              <div className="mt-1 text-xs text-ink-3">{a.arguments.reason}</div>
              <div className="mt-1 text-xs text-ink-3">Requested by {a.requested_by} · {fmtAgo(a.created_at)}</div>
              {canApprove ? (
                <div className="mt-2 flex gap-2">
                  <button className="btn-primary" onClick={() => decide.mutate({ id: a.action_id, d: "approve" })}><Check className="h-3.5 w-3.5" /> Approve</button>
                  <button className="btn-outline" onClick={() => decide.mutate({ id: a.action_id, d: "reject" })}><X className="h-3.5 w-3.5" /> Reject</button>
                </div>
              ) : <div className="mt-2 text-xs text-ink-3">A maintenance manager must approve.</div>}
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}
