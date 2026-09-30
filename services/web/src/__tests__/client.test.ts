import { afterEach, describe, expect, it, vi } from "vitest";
import { api, ApiError, qs, setToken, setUnauthorizedHandler } from "../api/client";

const json = (status: number, body: unknown, type = "application/problem+json") =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": type } });

afterEach(() => {
  vi.unstubAllGlobals();
  setToken(null);
  setUnauthorizedHandler(() => undefined);
});

describe("api client", () => {
  it("sends the bearer token and JSON body", async () => {
    const fetchMock = vi.fn().mockResolvedValue(json(200, { ok: true }, "application/json"));
    vi.stubGlobal("fetch", fetchMock);
    setToken("tok123");
    await expect(api("/alerts/x/ack", { method: "POST", json: { note: "hi" } })).resolves.toEqual({ ok: true });
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/v1/alerts/x/ack");
    expect(init.headers.get("Authorization")).toBe("Bearer tok123");
    expect(init.headers.get("Content-Type")).toBe("application/json");
    expect(init.body).toBe('{"note":"hi"}');
  });

  it("maps RFC 9457 problems to ApiError and signals 401", async () => {
    const onUnauth = vi.fn();
    setUnauthorizedHandler(onUnauth);
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(json(401, { title: "Unauthorized", detail: "token expired", request_id: "r1" })));
    const err = await api("/me").catch((e) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 401, title: "Unauthorized", message: "token expired", requestId: "r1" });
    expect(onUnauth).toHaveBeenCalledOnce();
  });

  it("returns undefined for 204", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(null, { status: 204 })));
    await expect(api("/x", { method: "DELETE" })).resolves.toBeUndefined();
  });

  it("builds query strings without empty values", () => {
    expect(qs({ a: 1, b: "", c: null, d: undefined, e: false, f: "x y" })).toBe("?a=1&e=false&f=x+y");
    expect(qs({})).toBe("");
  });
});
