import time

import jwt
import pytest

from app.domain.pagination import CursorError, clamp_limit, decode_cursor, encode_cursor
from app.security import rbac
from app.security.jwt import TokenService, generate_private_key_pem
from app.security.masking import geohash, geohash_center, mask_location, pseudonym

KEY = generate_private_key_pem()


def svc(ttl=60):
    return TokenService(KEY, "https://issuer.test", "fleetpulse-api", ttl)


def test_issue_and_verify_roundtrip():
    s = svc()
    tok, ttl = s.issue(user_id="u1", tenant_id="t1", email="a@b.c", name="A", roles=["analyst"])
    claims = s.verify(tok)
    assert claims["sub"] == "u1" and claims["tid"] == "t1" and claims["roles"] == ["analyst"]
    assert ttl == 3600
    assert jwt.get_unverified_header(tok)["kid"] == s.kid


def test_expired_token_rejected():
    s = svc(ttl=1)
    tok, _ = s.issue(user_id="u", tenant_id="t", email="e", name="n", roles=[], now=time.time() - 3600)
    with pytest.raises(jwt.ExpiredSignatureError):
        s.verify(tok)


def test_wrong_audience_and_issuer_rejected():
    a = svc()
    b = TokenService(KEY, "https://other", "other-aud", 60)
    tok, _ = b.issue(user_id="u", tenant_id="t", email="e", name="n", roles=[])
    with pytest.raises(jwt.PyJWTError):
        a.verify(tok)


def test_foreign_key_and_alg_none_rejected():
    a = svc()
    other = TokenService(generate_private_key_pem(), "https://issuer.test", "fleetpulse-api", 60)
    tok, _ = other.issue(user_id="u", tenant_id="t", email="e", name="n", roles=[])
    with pytest.raises(jwt.PyJWTError):
        a.verify(tok)
    unsigned = jwt.encode({"sub": "u", "aud": "fleetpulse-api", "iss": "https://issuer.test",
                           "exp": int(time.time()) + 60, "iat": int(time.time())}, None, algorithm="none")
    with pytest.raises(jwt.PyJWTError):
        a.verify(unsigned)


def test_jwks_shape():
    k = svc().jwks()["keys"][0]
    assert k["kty"] == "RSA" and k["alg"] == "RS256" and k["n"] and k["e"] == "AQAB"


def test_rbac_matrix():
    assert rbac.permissions_for(["platform_admin"]) == rbac.ALL
    assert rbac.PLATFORM_ADMIN not in rbac.permissions_for(["fleet_admin"])
    analyst = rbac.permissions_for(["analyst"])
    assert rbac.LOCATION_PRECISE not in analyst and rbac.DRIVER_PII not in analyst
    assert rbac.ALERT_ACK not in analyst and rbac.COPILOT_USE in analyst
    viewer = rbac.permissions_for(["viewer"])
    assert viewer == {rbac.FLEET_READ, rbac.VEHICLE_READ, rbac.ALERT_READ}
    assert rbac.permissions_for(["viewer", "analyst"]) == viewer | analyst
    assert rbac.permissions_for(["unknown"]) == frozenset()


@pytest.mark.parametrize("lat,lon,p,want", [(57.64911, 10.40744, 11, "u4pruydqqvj"), (42.605, -5.603, 5, "ezs42"), (0, 0, 1, "s")])
def test_geohash_reference_values(lat, lon, p, want):
    assert geohash(lat, lon, p) == want


def test_geohash_center_roundtrip_and_masking():
    lat, lon = 12.971598, 77.594566
    c = geohash_center(geohash(lat, lon, 9))
    assert abs(c[0] - lat) < 1e-4 and abs(c[1] - lon) < 1e-4
    mlat, mlon = mask_location(lat, lon, precise=False)
    assert (mlat, mlon) != (lat, lon)
    assert abs(mlat - lat) < 0.05 and abs(mlon - lon) < 0.05       # within the ~5 km cell
    assert mask_location(lat, lon, precise=True) == (lat, lon)
    assert mask_location(None, None, precise=False) == (None, None)


def test_pseudonym_stable_and_secret_dependent():
    assert pseudonym("d1", "s") == pseudonym("d1", "s")
    assert pseudonym("d1", "s") != pseudonym("d1", "t")
    assert pseudonym(None, "s") is None
    assert pseudonym("d1", "s").startswith("Driver #")


def test_cursor_roundtrip_and_tamper():
    c = encode_cursor({"vin": "ABC", "n": 3}, "secret")
    assert decode_cursor(c, "secret") == {"vin": "ABC", "n": 3}
    assert decode_cursor(None, "secret") is None
    with pytest.raises(CursorError):
        decode_cursor(c, "other-secret")
    with pytest.raises(CursorError):
        decode_cursor(c[:-2] + "AA", "secret")
    with pytest.raises(CursorError):
        decode_cursor("!!!", "secret")


def test_clamp_limit():
    assert clamp_limit(None) == 50 and clamp_limit(0) == 1 and clamp_limit(10_000) == 200 and clamp_limit(20) == 20
