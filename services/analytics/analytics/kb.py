"""Fault knowledge base for retrieval (pgvector).

Curated, synthetic service knowledge: what each fault code means, likely
causes, recommended actions and typical costs, plus workshop case notes.
Embedded with the shared hashing embedder and upserted idempotently.
"""

from __future__ import annotations

import uuid

import psycopg

from analytics.embeddings import embed, to_pgvector

NS = uuid.UUID("5d0f8c1e-2b4a-4d8e-9a61-7c3e2f1b9a44")

DTC_GUIDES = [
    ("P0217", "cooling", "Engine over-temperature (P0217)",
     "Coolant temperature exceeded the safe limit. Stop the vehicle safely and let it cool; continued driving can warp "
     "the cylinder head. Likely causes: low coolant from a leak, failed water pump, stuck thermostat, blocked radiator or "
     "failed cooling fan. Actions: pressure-test the cooling system, inspect pump and fan, check for head-gasket leak. "
     "Typical planned repair $300-900; roadside breakdown with tow and head damage $1,800-3,500."),
    ("P0128", "cooling", "Thermostat below regulating temperature (P0128)",
     "Engine takes too long to reach operating temperature. Usually a thermostat stuck open or a failing coolant "
     "temperature sensor. Often an early sign of cooling-system degradation. Replace thermostat and gasket, bleed the "
     "system. Typical cost $150-350."),
    ("P0480", "cooling", "Cooling fan control circuit (P0480)",
     "The ECU cannot drive cooling fan 1. Check fan relay, fuse, wiring and the fan motor. Overheating in slow traffic is "
     "the usual symptom. Typical cost $120-450."),
    ("P0115", "cooling", "Coolant temperature sensor circuit (P0115)",
     "Implausible coolant temperature signal. Inspect sensor connector and wiring; replace sensor. Typical cost $80-200."),
    ("P0562", "battery12v", "System voltage low (P0562)",
     "The 12 V system is below specification while running. Causes: weak battery, failing alternator or regulator, loose "
     "or corroded terminals, slipping belt. Load-test the battery and measure charging voltage (13.8-14.4 V expected). "
     "Planned replacement $150-400; a no-start roadside call with tow $500-800."),
    ("P0620", "battery12v", "Generator (alternator) control circuit (P0620)",
     "The ECU detects a fault in alternator control. Inspect alternator connector and field circuit; bench-test or "
     "replace the alternator. Typical cost $300-700."),
    ("P0300", "misfire", "Random/multiple cylinder misfire (P0300)",
     "Combustion misfires on more than one cylinder. Causes: worn spark plugs, failing coils, vacuum leaks, low fuel "
     "pressure or injector faults. Persistent misfire overheats the catalytic converter. Inspect plugs/coils, smoke-test "
     "intake, check fuel pressure. Planned repair $200-600; catalyst damage $1,200-2,500."),
    ("P0301", "misfire", "Cylinder 1 misfire (P0301)",
     "Misfire isolated to cylinder 1. Swap the coil to another cylinder to confirm; replace plug, coil or injector. "
     "Compression test if it persists. Typical cost $150-450."),
    ("P0302", "misfire", "Cylinder 2 misfire (P0302)",
     "Misfire isolated to cylinder 2. Same procedure as P0301: coil swap test, plug and injector inspection. $150-450."),
    ("P0A7F", "ev_pack", "Hybrid/EV battery pack deterioration (P0A7F)",
     "Pack capacity or internal resistance is out of range, often with rising cell temperature and faster state-of-charge "
     "drop. Run a pack health test, check cell balance and the thermal management loop. Early module replacement "
     "$1,500-3,000; full pack failure on the road $6,000-9,000."),
    ("P0AFA", "ev_pack", "Hybrid/EV battery system voltage low (P0AFA)",
     "HV pack voltage lower than expected. Check for weak modules, cell imbalance and contactor resistance. Schedule a "
     "balance and module test. $400-2,500 depending on findings."),
    ("P0A80", "ev_pack", "Replace hybrid/EV battery pack (P0A80)",
     "The battery management system has flagged the pack for replacement. Limit use, avoid fast charging, and book pack "
     "replacement or module repair. $4,000-9,000."),
    ("P0AA6", "ev_pack", "HV isolation fault (P0AA6)",
     "Insulation resistance between the HV system and chassis is low: a safety-critical condition. Vehicle must be "
     "inspected by an HV-certified technician before further use."),
    ("P0C73", "ev_pack", "Motor electronics coolant pump performance (P0C73)",
     "The inverter/motor coolant pump is underperforming, raising electronics and pack temperatures. Replace pump; check "
     "coolant level. $250-600."),
    ("P0442", "emissions", "Evaporative emission small leak (P0442)",
     "Minor EVAP leak, commonly a loose fuel cap or cracked hose. Not a breakdown risk; fix at next service. $20-250."),
    ("P0456", "emissions", "Evaporative emission very small leak (P0456)",
     "Very small EVAP leak. Low priority; check fuel cap seal and purge valve at next service."),
    ("P0171", "emissions", "System too lean, bank 1 (P0171)",
     "Air-fuel mixture lean: vacuum leak, dirty MAF sensor or weak fuel pump. Moderate priority."),
    ("P0420", "emissions", "Catalyst efficiency below threshold (P0420)",
     "Catalytic converter efficiency low, sometimes caused by earlier misfires. Plan replacement; verify O2 sensors."),
    ("U0100", "network", "Lost communication with engine ECU (U0100)",
     "CAN bus communication dropout. Often intermittent wiring or low system voltage. Check ground points and battery."),
    ("C0750", "chassis", "Tyre pressure sensor low pressure (C0750)",
     "A tyre is under-inflated. Inflate to specification and inspect for punctures; persistent loss indicates a slow leak."),
]

CASE_NOTES = [
    ("cooling", "Case note: van overheated on highway after a week of P0128",
     "Coolant averaged 6 °C above baseline for 5 days with intermittent P0128 before the vehicle overheated (P0217) on a "
     "delivery route. Root cause: water pump bearing failure. Lesson: a rising coolant trend plus thermostat codes should "
     "trigger an inspection within 48 hours."),
    ("cooling", "Case note: radiator fan relay failure in city traffic",
     "Repeated coolant peaks above 105 °C only at low speed; P0480 logged twice. Replaced fan relay ($45) before any damage."),
    ("battery12v", "Case note: no-start after voltage sag",
     "Charging voltage drifted from 14.0 V to 13.2 V over ten days with occasional P0562. Alternator diode failure. "
     "Planned alternator replacement costs roughly a third of a roadside no-start with tow."),
    ("battery12v", "Case note: corroded terminal mimicking alternator fault",
     "Low minimum voltage spikes during cranking only; cleaning and re-torquing terminals resolved it for $30."),
    ("misfire", "Case note: coil pack failure on a high-mileage van",
     "Misfire codes P0301/P0300 appeared a few times per day for a week, then the vehicle entered limp mode. Coil pack and "
     "plugs replaced. Early replacement avoids catalytic converter damage."),
    ("ev_pack", "Case note: EV pack thermal loop degradation",
     "HV pack ran 8-10 °C hotter than fleet peers during fast charging with P0A7F appearing twice. Coolant pump for the "
     "battery loop was failing; replaced before cell damage."),
    ("ev_pack", "Case note: cell imbalance caught early",
     "Faster state-of-charge drop per km and P0AFA. Module balancing restored range; avoided a $7,000 pack replacement."),
    ("general", "Maintenance policy: acting on 7-day breakdown predictions",
     "Vehicles with predicted breakdown risk above 50% should be inspected within 2 days (P1); 25-50% within the week (P2). "
     "Planned repairs cost 20-35% of an unplanned roadside breakdown once towing, downtime and secondary damage are included."),
    ("general", "Idling policy",
     "Engine idling burns roughly 0.8-1.0 litres of fuel per hour for light commercial vehicles. Auto stop-start, driver "
     "coaching and idle alerts after 10 minutes typically cut idle time by 25-40%."),
]


def documents() -> list[dict]:
    docs = [{"dtc_code": code, "component": comp, "title": title, "content": text, "source": "FleetPulse service guide"}
            for code, comp, title, text in DTC_GUIDES]
    docs += [{"dtc_code": None, "component": comp, "title": title, "content": text, "source": "Workshop case notes"}
             for comp, title, text in CASE_NOTES]
    for d in docs:
        d["kb_id"] = str(uuid.uuid5(NS, d["title"]))
    return docs


def build(conn: psycopg.Connection) -> int:
    docs = documents()
    with conn.cursor() as cur:
        for d in docs:
            vec = to_pgvector(embed(f"{d['title']} {d['dtc_code'] or ''} {d['component']} {d['content']}"))
            cur.execute(
                """INSERT INTO fault_knowledge (kb_id, dtc_code, component, title, content, source, embedding)
                   VALUES (%s, %s, %s, %s, %s, %s, %s::vector)
                   ON CONFLICT (kb_id) DO UPDATE SET content = EXCLUDED.content, embedding = EXCLUDED.embedding,
                       title = EXCLUDED.title, component = EXCLUDED.component""",
                (d["kb_id"], d["dtc_code"], d["component"], d["title"], d["content"], d["source"], vec))
    conn.commit()
    return len(docs)
