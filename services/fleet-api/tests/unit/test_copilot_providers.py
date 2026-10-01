"""Copilot provider selection and the OpenAI-compatible tool-use loop, against
a mocked chat-completions server (the wire format used by Groq, Google
Gemini's OpenAI endpoint, OpenRouter and Ollama)."""

from __future__ import annotations

import json
from types import SimpleNamespace

import fakeredis.aioredis
import httpx
import pytest

from app.agent import copilot as cp
from app.config import Settings
from app.security import rbac
from app.security.jwt import Principal

MAINT = Principal("u1", "t1", "m@x", "M", ("maintenance_manager",), rbac.permissions_for(["maintenance_manager"]))
VIN = "1HGCM82633A004352"


def settings(**kw) -> Settings:
    base = {"llm_provider": "auto", "llm_base_url": "", "llm_api_key": "", "anthropic_api_key": "",
            "llm_model": "test-model"}
    return Settings(**{**base, **kw})


class Audit:
    def __init__(self) -> None:
        self.events = []

    def record(self, ev) -> None:
        self.events.append(ev)


def make_copilot(s: Settings, responses, seen: list) -> cp.Copilot:
    """Copilot whose HTTP client replays `responses` and records requests."""
    ctx = SimpleNamespace(settings=s, audit=Audit(), redis=fakeredis.aioredis.FakeRedis())
    c = cp.Copilot(ctx)
    it = iter(responses)

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append({"url": str(request.url), "auth": request.headers.get("authorization"),
                     "body": json.loads(request.content)})
        status, body = next(it)
        return httpx.Response(status, json=body)

    c.http = httpx.AsyncClient(base_url=s.llm_base_url, transport=httpx.MockTransport(handler),
                               headers={"Authorization": f"Bearer {s.llm_api_key}"})
    return c


def tool_call(cid, name, args) -> dict:
    return {"id": cid, "type": "function", "function": {"name": name, "arguments": json.dumps(args)}}


def reply(content=None, calls=None, finish="stop", tokens=(100, 20)) -> tuple[int, dict]:
    msg = {"role": "assistant", "content": content}
    if calls:
        msg["tool_calls"] = calls
        finish = "tool_calls"
    return 200, {"choices": [{"message": msg, "finish_reason": finish}],
                 "usage": {"prompt_tokens": tokens[0], "completion_tokens": tokens[1]}}


@pytest.fixture
def fake_tools(monkeypatch):
    executed = []

    async def run_tool(tc, name, args):
        executed.append((name, args))
        if name == "list_at_risk_vehicles":
            return {"vehicles": [{"vin": VIN, "risk_7d": 0.93, "component": "cooling"}]}, False
        return {"error": f"unknown tool {name}"}, True

    monkeypatch.setattr(cp, "run_tool", run_tool)
    return executed


@pytest.mark.parametrize("kw,expected", [
    ({}, "offline"),
    ({"anthropic_api_key": "sk"}, "anthropic"),
    ({"llm_base_url": "https://api.groq.com/openai/v1"}, "openai"),
    ({"llm_base_url": "https://x/v1", "anthropic_api_key": "sk"}, "openai"),       # explicit endpoint wins
    ({"llm_provider": "anthropic", "llm_base_url": "https://x/v1", "anthropic_api_key": "sk"}, "anthropic"),
    ({"llm_provider": "openai"}, "offline"),                                      # missing base URL
    ({"llm_provider": "offline", "anthropic_api_key": "sk"}, "offline"),
])
def test_provider_resolution(kw, expected):
    assert cp.resolve_provider(settings(**kw)) == expected


def test_claude_model_name_with_other_provider_falls_back_offline():
    ctx = SimpleNamespace(settings=settings(llm_base_url="https://x/v1", llm_model="claude-opus-5-5"),
                          audit=Audit(), redis=None)
    assert cp.Copilot(ctx).provider == "offline"


async def test_openai_compatible_tool_loop(fake_tools):
    s = settings(llm_base_url="https://llm.test/v1", llm_api_key="free-key", llm_model="llama-test")
    seen: list = []
    c = make_copilot(s, [
        reply(calls=[tool_call("call_1", "list_at_risk_vehicles", {"min_risk": 0.3, "component": "any", "limit": 5})]),
        reply(content=f"`{VIN}` is most at risk (93%, cooling).", tokens=(300, 40)),
    ], seen)
    out = await c._openai_turn(cp.ToolContext(c.ctx, MAINT, "conv1"), [], "Which vehicles will break down?")

    assert out.answer.startswith(f"`{VIN}`") and out.mode == "llm"
    assert fake_tools == [("list_at_risk_vehicles", {"min_risk": 0.3, "component": "any", "limit": 5})]
    first, second = seen[0], seen[1]
    assert first["url"] == "https://llm.test/v1/chat/completions" and first["auth"] == "Bearer free-key"
    assert first["body"]["model"] == "llama-test" and first["body"]["messages"][0]["role"] == "system"
    names = {t["function"]["name"] for t in first["body"]["tools"]}
    assert "propose_work_order" in names and "list_at_risk_vehicles" in names    # maintenance manager's tools
    assert all(t["type"] == "function" and "parameters" in t["function"] for t in first["body"]["tools"])
    tool_msg = second["body"]["messages"][-1]
    assert tool_msg["role"] == "tool" and tool_msg["tool_call_id"] == "call_1" and VIN in tool_msg["content"]
    assert second["body"]["messages"][-2]["tool_calls"][0]["id"] == "call_1"
    assert out.usage["input_tokens"] == 400 and out.usage["output_tokens"] == 60
    assert out.usage["provider"] == "openai" and out.usage["est_cost_usd"] == 0
    assert [e.action for e in c.ctx.audit.events] == ["copilot.tool.list_at_risk_vehicles"]


async def test_paid_endpoint_prices_apply_when_configured(fake_tools):
    s = settings(llm_base_url="https://llm.test/v1", llm_usd_per_mtok_in=1.0, llm_usd_per_mtok_out=2.0)
    c = make_copilot(s, [reply(content="ok", tokens=(1_000_000, 500_000))], [])
    out = await c._openai_turn(cp.ToolContext(c.ctx, MAINT, "c"), [], "hi")
    assert out.usage["est_cost_usd"] == 2.0


async def test_viewer_is_not_offered_write_tools(fake_tools):
    viewer = Principal("u2", "t1", "v@x", "V", ("viewer",), rbac.permissions_for(["viewer"]))
    seen: list = []
    c = make_copilot(settings(llm_base_url="https://llm.test/v1"), [reply(content="ok")], seen)
    await c._openai_turn(cp.ToolContext(c.ctx, viewer, "conv2"), [], "hi")
    names = {t["function"]["name"] for t in seen[0]["body"]["tools"]}
    assert "propose_work_order" not in names and "list_at_risk_vehicles" not in names


async def test_bad_arguments_missing_ids_and_budget(fake_tools):
    s = settings(llm_base_url="https://llm.test/v1", llm_max_tool_calls=1)
    seen: list = []
    broken = {"type": "function", "function": {"name": "list_at_risk_vehicles", "arguments": "{not json"}}
    c = make_copilot(s, [
        reply(calls=[broken]),                                                    # no id, invalid JSON
        reply(calls=[tool_call("c2", "list_at_risk_vehicles", {}), tool_call("c3", "get_fleet_summary", {})]),
        reply(content="Done with what I have."),
    ], seen)
    out = await c._openai_turn(cp.ToolContext(c.ctx, MAINT, "conv3"), [], "rank")
    assert fake_tools == []                                     # invalid args never reach a tool
    second_msgs = seen[1]["body"]["messages"]
    assert second_msgs[-2]["tool_calls"][0]["id"] == second_msgs[-1]["tool_call_id"]   # generated id matches
    assert second_msgs[-1]["content"].startswith("error: arguments were not valid JSON")
    third_msgs = seen[2]["body"]["messages"]
    assert [m["tool_call_id"] for m in third_msgs[-2:]] == ["c2", "c3"]
    assert all("budget" in m["content"] for m in third_msgs[-2:])
    assert "tool-call budget reached" in out.warnings and out.answer == "Done with what I have."


async def test_provider_error_falls_back_to_offline_planner(monkeypatch, fake_tools):
    seen: list = []
    c = make_copilot(settings(llm_base_url="https://llm.test/v1"), [(429, {"error": "rate limited"})], seen)

    async def offline(tc, message):
        return cp.TurnResult("", "offline answer", "offline", warnings=["Answered by the offline planner."])

    monkeypatch.setattr(c, "_offline_turn", offline)
    out = await c.chat(MAINT, "Which vehicles will break down?", None, "req-1")
    assert out.mode == "offline" and out.answer == "offline answer"
    assert any("offline planner" in w for w in out.warnings)
    assert len(seen) == 1


KNOWN = {"list_at_risk_vehicles", "get_fleet_summary"}


@pytest.mark.parametrize("text,name,args,rest", [
    ('<function=list_at_risk_vehicles>{"min_risk": 0.3}</function>', "list_at_risk_vehicles", {"min_risk": 0.3}, ""),
    ('Checking.<function=get_fleet_summary>{}</function>', "get_fleet_summary", {}, "Checking."),
    ('<function=list_at_risk_vehicles>{"limit": 5}<function>', "list_at_risk_vehicles", {"limit": 5}, ""),
    ('<function/get_fleet_summary>{"a": {"b": 1}}</function>', "get_fleet_summary", {"a": {"b": 1}}, ""),
    ('<function=list_at_risk_vehicles{"limit": 2}</function>', "list_at_risk_vehicles", {"limit": 2}, ""),
    ('<|python_tag|>{"name": "get_fleet_summary", "parameters": {}}', "get_fleet_summary", {}, ""),
    ('{"type": "function", "name": "list_at_risk_vehicles", "parameters": {"limit": 1}}',
     "list_at_risk_vehicles", {"limit": 1}, ""),
])
def test_text_tool_calls_are_recovered(text, name, args, rest):
    calls, remaining = cp.text_tool_calls(text, KNOWN)
    assert [(c["function"]["name"], json.loads(c["function"]["arguments"])) for c in calls] == [(name, args)]
    assert remaining == rest


@pytest.mark.parametrize("text", [
    "Plain answer with {braces} in it.",
    '<function=drop_tables>{"all": true}</function>',      # not an offered tool: stays text
    '{"name": "get_fleet_summary"}',                       # no arguments object
    '<function=get_fleet_summary>{not json}</function>',
])
def test_text_without_valid_calls_is_left_alone(text):
    assert cp.text_tool_calls(text, KNOWN) == ([], text)


async def test_tool_call_written_as_text_is_executed(fake_tools):
    s = settings(llm_base_url="https://llm.test/v1")
    seen: list = []
    c = make_copilot(s, [
        reply(content='<function=list_at_risk_vehicles>{"min_risk": 0.5, "limit": 3}</function>'),
        reply(content=f"`{VIN}` first.<|eot_id|>"),
    ], seen)
    out = await c._openai_turn(cp.ToolContext(c.ctx, MAINT, "conv4"), [], "Which vehicles will break down?")
    assert fake_tools == [("list_at_risk_vehicles", {"min_risk": 0.5, "limit": 3})]
    assistant, tool = seen[1]["body"]["messages"][-2:]
    assert assistant["content"] is None and assistant["tool_calls"][0]["id"] == tool["tool_call_id"]
    assert out.answer == f"`{VIN}` first."                    # leftover special tokens stripped


@pytest.mark.parametrize("final", ["", "<function=unknown_tool>{}</function>", "<|eot_id|>"])
async def test_no_usable_answer_falls_back_offline(monkeypatch, fake_tools, final):
    c = make_copilot(settings(llm_base_url="https://llm.test/v1"), [reply(content=final)], [])

    async def offline(tc, message):
        return cp.TurnResult("", "offline answer", "offline", warnings=["Answered by the offline planner."])

    monkeypatch.setattr(c, "_offline_turn", offline)
    out = await c.chat(MAINT, "Which vehicles will break down?", None, "req-2")
    assert out.mode == "offline" and out.answer == "offline answer"


async def test_endless_tool_calls_fall_back_offline(monkeypatch, fake_tools):
    s = settings(llm_base_url="https://llm.test/v1", llm_max_tool_calls=1)
    loop = [reply(calls=[tool_call(f"c{i}", "list_at_risk_vehicles", {})]) for i in range(3)]
    c = make_copilot(s, loop, [])

    async def offline(tc, message):
        return cp.TurnResult("", "offline answer", "offline")

    monkeypatch.setattr(c, "_offline_turn", offline)
    out = await c.chat(MAINT, "rank", None, "req-3")
    assert out.mode == "offline"
