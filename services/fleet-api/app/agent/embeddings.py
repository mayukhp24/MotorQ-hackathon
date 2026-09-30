"""Deterministic local text embedding (signed feature hashing).

256-d vectors from word unigrams, bigrams and DTC codes, L2-normalised.
Needs no model download or external API, so retrieval works offline and is
identical in the analytics job (which embeds the knowledge base) and the API
(which embeds queries). Contract: tests/unit/test_embeddings.py pins vectors
that services/analytics must reproduce byte-for-byte.
"""

from __future__ import annotations

import hashlib
import math
import re

DIM = 256
_TOKEN = re.compile(r"[a-z0-9]+")
_DTC = re.compile(r"\b[pcbu][0-3][0-9a-f]{3}\b")
_STOP = frozenset("a an the of to and or in on for is are be with at by from this that it as its into can may".split())


def _tokens(text: str) -> list[str]:
    t = text.lower()
    words = [w for w in _TOKEN.findall(t) if w not in _STOP]
    feats = list(words)
    feats += [f"{a}_{b}" for a, b in zip(words, words[1:])]
    feats += [f"dtc:{c}" for c in _DTC.findall(t)] * 3  # codes are strong signals
    return feats


def embed(text: str) -> list[float]:
    vec = [0.0] * DIM
    for f in _tokens(text):
        h = hashlib.blake2b(f.encode(), digest_size=8).digest()
        idx = int.from_bytes(h[:4], "little") % DIM
        sign = 1.0 if h[4] & 1 else -1.0
        vec[idx] += sign
    norm = math.sqrt(sum(v * v for v in vec)) or 1.0
    return [round(v / norm, 6) for v in vec]


def to_pgvector(vec: list[float]) -> str:
    return "[" + ",".join(f"{v:.6f}" for v in vec) + "]"
