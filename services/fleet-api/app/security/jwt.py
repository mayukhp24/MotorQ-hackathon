"""RS256 JWT issuance/verification with a published JWKS.

The API is an OAuth2 resource server. Locally it also acts as a minimal
authorisation server (password grant) so the stack runs without an external
IdP; in production point JWKS at Keycloak/Auth0/Entra and disable local
issuance. Tokens are short-lived and carry tenant and roles as claims.
"""

from __future__ import annotations

import base64
import time
import uuid
from dataclasses import dataclass, field

import jwt
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import rsa


def _b64u(n: int) -> str:
    b = n.to_bytes((n.bit_length() + 7) // 8, "big")
    return base64.urlsafe_b64encode(b).rstrip(b"=").decode()


def generate_private_key_pem() -> str:
    key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    return key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8,
                             serialization.NoEncryption()).decode()


@dataclass(frozen=True)
class Principal:
    user_id: str
    tenant_id: str | None
    email: str
    name: str
    roles: tuple[str, ...]
    permissions: frozenset[str] = field(default_factory=frozenset)
    token_id: str = ""

    def can(self, perm: str) -> bool:
        return perm in self.permissions


class TokenService:
    def __init__(self, private_key_pem: str, issuer: str, audience: str, ttl_minutes: int) -> None:
        self._key = serialization.load_pem_private_key(private_key_pem.encode(), password=None)
        self._pub = self._key.public_key()
        nums = self._pub.public_numbers()
        der = self._pub.public_bytes(serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo)
        self.kid = base64.urlsafe_b64encode(uuid.uuid5(uuid.NAMESPACE_OID, der.hex()).bytes).rstrip(b"=").decode()[:16]
        self.jwk = {"kty": "RSA", "use": "sig", "alg": "RS256", "kid": self.kid, "n": _b64u(nums.n), "e": _b64u(nums.e)}
        self.issuer, self.audience, self.ttl = issuer, audience, ttl_minutes * 60

    def issue(self, *, user_id: str, tenant_id: str | None, email: str, name: str, roles: list[str],
              now: float | None = None) -> tuple[str, int]:
        now = now or time.time()
        claims = {
            "iss": self.issuer, "aud": self.audience, "sub": user_id, "tid": tenant_id,
            "email": email, "name": name, "roles": roles,
            "iat": int(now), "nbf": int(now) - 5, "exp": int(now) + self.ttl, "jti": uuid.uuid4().hex,
        }
        token = jwt.encode(claims, self._key, algorithm="RS256", headers={"kid": self.kid})
        return token, self.ttl

    def verify(self, token: str) -> dict:
        # Algorithm is pinned (no "none"/HS256 confusion); iss/aud/exp are enforced.
        return jwt.decode(token, self._pub, algorithms=["RS256"], audience=self.audience, issuer=self.issuer,
                          options={"require": ["exp", "iat", "sub", "iss", "aud"]}, leeway=10)

    def jwks(self) -> dict:
        return {"keys": [self.jwk]}
