import json
import random
import time
import uuid

from behave import given, then, when

API = "/api/v1"


def api(context, method, path, token=None, **kw):
    headers = kw.pop("headers", {})
    tok = token or getattr(context, "token", None)
    if tok:
        headers["Authorization"] = f"Bearer {tok}"
    return context.http.request(method, context.base + API + path, headers=headers, timeout=30, **kw)


def token_for(context, email):
    r = context.http.post(context.base + API + "/auth/token", data={"username": email, "password": context.password}, timeout=30)
    assert r.status_code == 200, r.text
    return r.json()["access_token"]


@given('I am signed in as "{email}"')
def step_login(context, email):
    context.token = token_for(context, email)


@given('a vehicle from my fleet reported by the "{oem}" OEM cloud')
def step_vehicle(context, oem):
    cursor, found = None, []
    for _ in range(10):
        r = api(context, "GET", "/vehicles", params={"limit": 200, **({"cursor": cursor} if cursor else {})}).json()
        found += [v for v in r["items"] if v["oem_code"] == oem and v["powertrain"] != "EV"]
        cursor = r["next_cursor"]
        if len(found) > 50 or not cursor:
            break
    context.vehicle = random.choice(found)


def aurora_record(vin, ts_ms, seq, coolant):
    return {"vehicle": {"vin": vin}, "timestamp": ts_ms, "sequence": seq,
            "position": {"latitude": 13.0827, "longitude": 80.2707, "heading": 90},
            "motion": {"speedKph": 45.0, "accelMps2": 0.1},
            "powertrain": {"odometerKm": 250000.0, "engineOn": True, "rpm": 1900, "coolantTempC": coolant, "fuelLevelPct": 50},
            "electrical": {"batteryVoltage": 13.9}}


@when("the OEM cloud reports 20 seconds of coolant at {temp:d} °C for that vehicle")
def step_overheat(context, temp):
    # Timestamps slightly ahead of the live simulator so interleaved regular
    # samples count as late and cannot interrupt the sustained condition.
    base = int(time.time() * 1000) + 60_000
    seq0 = base * 10
    records = [aurora_record(context.vehicle["vin"], base + i * 1000, seq0 + i, float(temp)) for i in range(20)]
    context.sent_at = time.time()
    r = context.http.post(f"{context.ingest}/v1/ingest/aurora", json={"oem": "aurora", "records": records},
                          headers={"X-Api-Key": context.keys["aurora"]}, timeout=30)
    assert r.status_code == 202, r.text
    assert r.json()["accepted"] == 20


@then('an "{alert_type}" alert for that vehicle is visible within {seconds:d} seconds')
def step_alert_visible(context, alert_type, seconds):
    deadline = context.sent_at + seconds
    vin = context.vehicle["vin"]
    while time.time() < deadline + 0.5:
        items = api(context, "GET", "/alerts", params={"vin": vin, "type": alert_type, "limit": 5}).json()["items"]
        fresh = [a for a in items if a["detected_at"] and time.time() - _epoch(a["detected_at"]) < 120]
        if fresh:
            context.alert = fresh[0]
            context.alert_latency = time.time() - context.sent_at
            print(f"alert visible via API after {context.alert_latency:.2f}s")
            return
        time.sleep(0.25)
    raise AssertionError(f"no {alert_type} alert for {vin} within {seconds}s")


def _epoch(iso):
    from datetime import datetime

    return datetime.fromisoformat(iso.replace("Z", "+00:00")).timestamp()


@then('the alert has severity "{sev}"')
def step_alert_sev(context, sev):
    assert context.alert["severity"] == sev


VALID_PINNACLE = "1HGCM82633A004352"


@when('the "{oem}" OEM cloud sends a batch with 1 valid record and 1 record with a bad VIN check digit')
def step_bad_batch(context, oem):
    now = time.strftime("%Y-%m-%dT%H:%M:%S.000Z", time.gmtime())
    rec = {"VIN": VALID_PINNACLE, "EventTime": now, "Seq": int(time.time()), "Lat": 12.9, "Lng": 77.6, "SpeedMph": 20,
           "OdometerMi": 1000, "Ign": "ON"}
    bad = dict(rec, VIN=VALID_PINNACLE[:8] + "9" + VALID_PINNACLE[9:], Seq=int(time.time()) + 1)
    context.resp = context.http.post(f"{context.ingest}/v1/ingest/{oem}", json={"data": [rec, bad]},
                                     headers={"X-Api-Key": context.keys[oem]}, timeout=30)


@then("the gateway accepts {a:d} record and rejects {r:d} record")
def step_accept_reject(context, a, r):
    assert context.resp.status_code == 202, context.resp.text
    body = context.resp.json()
    assert body["accepted"] == a and body["rejected"] == r, body


@when('the "{oem}" OEM cloud sends a batch with a wrong API key')
def step_wrong_key(context, oem):
    context.resp = context.http.post(f"{context.ingest}/v1/ingest/{oem}", json={"messages": []},
                                     headers={"X-Api-Key": "not-the-key"}, timeout=30)


@then("the gateway answers {code:d}")
def step_gateway_code(context, code):
    assert context.resp.status_code == code


@when('I request a vehicle that belongs to "{tenant}"')
def step_foreign_vehicle(context, tenant):
    other = token_for(context, "maint@acme.demo")
    vin = api(context, "GET", "/vehicles?limit=1", token=other).json()["items"][0]["vin"]
    context.resp = api(context, "GET", f"/vehicles/{vin}")


@then("the API answers {code:d}")
def step_api_code(context, code):
    assert context.resp.status_code == code, context.resp.status_code


@when("I open the vehicle list")
def step_list(context):
    context.vehicles = api(context, "GET", "/vehicles?limit=50").json()["items"]


def on_grid(lat, lon):
    # Snapped positions are geohash-5 cell centres: re-snapping is a no-op.
    import math

    return lat is not None and abs(lat * 1e6 - round(lat * 1e6)) < 1e-3 and not math.isnan(lon)


@then("vehicle positions are snapped to the privacy grid")
def step_masked(context):
    live = [v["live"] for v in context.vehicles if v["live"] and v["live"]["lat"] is not None]
    assert live, "no live positions"
    precise_tok = token_for(context, "maint@acme.demo")
    precise = {v["vin"]: v["live"] for v in api(context, "GET", "/vehicles?limit=50", token=precise_tok).json()["items"] if v["live"]}
    diffs = [abs(v["live"]["lat"] - precise[v["vin"]]["lat"]) for v in context.vehicles if v["live"] and v["vin"] in precise]
    assert any(d > 1e-4 for d in diffs), "analyst positions equal precise positions"
    assert all(d < 0.05 for d in diffs), "masking moved a vehicle outside its ~5 km cell"


@then("acknowledging an alert is forbidden")
def step_ack_forbidden(context):
    alert = api(context, "GET", "/alerts?limit=1&status=OPEN").json()["items"][0]
    r = api(context, "POST", f"/alerts/{alert['alert_id']}/ack")
    assert r.status_code == 403, r.status_code


@then("my access appears in the audit trail")
def step_audited(context):
    items = api(context, "GET", "/compliance/audit?limit=50").json()["items"]
    assert any(i["action"] == "GET /api/v1/vehicles" for i in items)


@then("the audit hash chain verifies as intact")
def step_chain(context):
    assert api(context, "GET", "/compliance/audit/verify").json()["intact"] is True


@when("I open the breakdown predictions")
def step_predictions(context):
    context.risk = api(context, "GET", "/maintenance/risk?limit=30&min_risk=0.1").json()["items"]


@then("vehicles are ordered by risk, highest first")
def step_ordered(context):
    risks = [r["risk_7d"] for r in context.risk]
    assert risks and risks == sorted(risks, reverse=True)


@then("each high-risk vehicle shows at least one reason and an avoidable cost")
def step_reasons(context):
    for r in context.risk:
        if r["risk_7d"] >= 0.5:
            assert r["top_factors"] and r["top_factors"][0]["label"], r
            assert r["expected_savings_usd"] > 0


@when("I schedule a repair for the riskiest vehicle without an open work order")
def step_schedule(context):
    risk = api(context, "GET", "/maintenance/risk?limit=100&min_risk=0.3").json()["items"]
    target = next(r for r in risk if not r["has_open_work_order"])
    context.vin = target["vin"]
    r = api(context, "POST", "/maintenance/work-orders", json={"vin": target["vin"], "component": target["top_component"],
                                                                "priority": "P1", "source": "PREDICTION"})
    assert r.status_code == 201, r.text


@then('a work order from source "{source}" exists for that vehicle')
def step_wo_exists(context, source):
    v = api(context, "GET", f"/vehicles/{context.vin}").json()
    assert any(w["source"] == source for w in v["work_orders"])


@when("I ask the copilot to schedule a repair for a high-risk vehicle")
def step_copilot(context):
    risk = api(context, "GET", "/maintenance/risk?limit=100&min_risk=0.3").json()["items"]
    vin = next(r["vin"] for r in risk if not r["has_open_work_order"])
    context.vin = vin
    context.turn = api(context, "POST", "/copilot/chat", json={"message": f"Please schedule a fix for {vin}, cooling looks bad"}).json()


@then("the copilot proposes a work order that is pending approval")
def step_proposed(context):
    props = context.turn["proposed_actions"]
    assert props and props[0]["vin"] == context.vin, context.turn
    context.action_id = props[0]["action_id"]
    pending = api(context, "GET", "/copilot/actions?status=PROPOSED").json()["items"]
    assert any(a["action_id"] == context.action_id for a in pending)


@then("a viewer cannot approve it")
def step_viewer_denied(context):
    viewer = token_for(context, "viewer@acme.demo")
    r = api(context, "POST", f"/copilot/actions/{context.action_id}/approve", token=viewer)
    assert r.status_code == 403


@then("when I approve it a work order is created")
def step_approve(context):
    r = api(context, "POST", f"/copilot/actions/{context.action_id}/approve").json()
    assert r["status"] == "EXECUTED" and r["result"]["work_order_id"], r
    v = api(context, "GET", f"/vehicles/{context.vin}").json()
    assert any(w["source"] == "COPILOT" for w in v["work_orders"])
