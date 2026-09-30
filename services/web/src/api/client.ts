// Thin fetch wrapper: bearer auth, JSON, RFC 9457 problem+json errors.

export class ApiError extends Error {
  constructor(public status: number, public title: string, public detail?: string, public requestId?: string) {
    super(detail || title);
  }
}

let token: string | null = sessionStorage.getItem("fp.token");
let onUnauthorized: (() => void) | null = null;

export function setToken(t: string | null) {
  token = t;
  if (t) sessionStorage.setItem("fp.token", t);
  else sessionStorage.removeItem("fp.token");
}
export const getToken = () => token;
export function setUnauthorizedHandler(fn: () => void) {
  onUnauthorized = fn;
}

export async function api<T = unknown>(path: string, init: RequestInit & { json?: unknown } = {}): Promise<T> {
  const headers = new Headers(init.headers);
  if (token) headers.set("Authorization", `Bearer ${token}`);
  let body = init.body;
  if (init.json !== undefined) {
    headers.set("Content-Type", "application/json");
    body = JSON.stringify(init.json);
  }
  const res = await fetch(`/api/v1${path}`, { ...init, headers, body });
  if (res.status === 204) return undefined as T;
  const isJson = (res.headers.get("content-type") || "").includes("json");
  const data = isJson ? await res.json() : await res.text();
  if (!res.ok) {
    if (res.status === 401 && onUnauthorized) onUnauthorized();
    const p = (isJson ? data : {}) as { title?: string; detail?: string; request_id?: string };
    throw new ApiError(res.status, p.title || res.statusText, p.detail, p.request_id);
  }
  return data as T;
}

export async function login(email: string, password: string) {
  const form = new URLSearchParams({ username: email, password, grant_type: "password" });
  const res = await fetch("/api/v1/auth/token", { method: "POST", body: form });
  const data = await res.json();
  if (!res.ok) throw new ApiError(res.status, data.title || "Login failed", data.detail);
  return data as { access_token: string; expires_in: number; user: Me };
}

export interface Me {
  user_id: string;
  email: string;
  name: string;
  tenant_id: string | null;
  tenant_name?: string | null;
  roles: string[];
  permissions: string[];
}

export interface Page<T> {
  items: T[];
  next_cursor: string | null;
}

export function qs(params: Record<string, string | number | boolean | null | undefined>) {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== null && v !== "") p.set(k, String(v));
  const s = p.toString();
  return s ? `?${s}` : "";
}
