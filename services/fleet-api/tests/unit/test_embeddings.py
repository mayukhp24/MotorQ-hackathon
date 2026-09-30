import hashlib
import importlib.util
import math

from app.agent.embeddings import DIM, embed, to_pgvector
from tests.conftest import REPO


def cos(a, b):
    return sum(x * y for x, y in zip(a, b))


def test_embedding_is_normalised_and_deterministic():
    v = embed("Engine coolant over-temperature P0217")
    assert len(v) == DIM
    assert abs(math.sqrt(sum(x * x for x in v)) - 1) < 1e-5
    assert v == embed("Engine coolant over-temperature P0217")


def test_related_text_scores_higher():
    q = embed("coolant temperature too high, engine overheating P0217")
    near = embed("Engine over-temperature (P0217): coolant exceeded the safe limit; check water pump and radiator")
    far = embed("Tyre pressure sensor low pressure: inflate tyre to specification")
    assert cos(q, near) > cos(q, far) + 0.1


def test_pgvector_literal():
    s = to_pgvector([0.5, -0.25])
    assert s == "[0.500000,-0.250000]"


def test_contract_api_and_analytics_embed_identically():
    """Consumer/producer contract: the analytics job embeds the knowledge
    base, the API embeds queries; both must produce identical vectors."""
    path = REPO / "services" / "analytics" / "analytics" / "embeddings.py"
    spec = importlib.util.spec_from_file_location("analytics_embeddings", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)  # type: ignore[union-attr]
    for text in ["P0562 system voltage low", "HV pack deterioration P0A7F", "idling policy"]:
        assert mod.embed(text) == embed(text)
    digest = hashlib.sha256(str(embed("fleetpulse contract probe P0301")).encode()).hexdigest()
    assert len(digest) == 64
