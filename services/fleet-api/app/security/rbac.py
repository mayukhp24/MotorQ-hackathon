"""Role-based access control: roles map to fine-grained permissions."""

from __future__ import annotations

FLEET_READ = "fleet:read"
VEHICLE_READ = "vehicle:read"
LOCATION_PRECISE = "location:precise"
DRIVER_PII = "driver:pii"
ALERT_READ = "alert:read"
ALERT_ACK = "alert:ack"
MAINT_READ = "maintenance:read"
MAINT_WRITE = "maintenance:write"
ANALYTICS_READ = "analytics:read"
COPILOT_USE = "copilot:use"
COPILOT_APPROVE = "copilot:approve"
AUDIT_READ = "audit:read"
PRIVACY_ERASE = "privacy:erase"
PLATFORM_ADMIN = "platform:admin"

ALL = frozenset({FLEET_READ, VEHICLE_READ, LOCATION_PRECISE, DRIVER_PII, ALERT_READ, ALERT_ACK, MAINT_READ,
                 MAINT_WRITE, ANALYTICS_READ, COPILOT_USE, COPILOT_APPROVE, AUDIT_READ, PRIVACY_ERASE, PLATFORM_ADMIN})

ROLE_PERMISSIONS: dict[str, frozenset[str]] = {
    "platform_admin": ALL,
    "fleet_admin": ALL - {PLATFORM_ADMIN},
    "maintenance_manager": frozenset({FLEET_READ, VEHICLE_READ, LOCATION_PRECISE, ALERT_READ, ALERT_ACK, MAINT_READ,
                                      MAINT_WRITE, ANALYTICS_READ, COPILOT_USE, COPILOT_APPROVE}),
    "analyst": frozenset({FLEET_READ, VEHICLE_READ, ALERT_READ, MAINT_READ, ANALYTICS_READ, COPILOT_USE}),
    "viewer": frozenset({FLEET_READ, VEHICLE_READ, ALERT_READ}),
}


def permissions_for(roles: list[str] | tuple[str, ...]) -> frozenset[str]:
    out: set[str] = set()
    for r in roles:
        out |= ROLE_PERMISSIONS.get(r, frozenset())
    return frozenset(out)
