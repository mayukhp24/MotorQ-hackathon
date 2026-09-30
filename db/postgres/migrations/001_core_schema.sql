-- FleetPulse relational core (PostgreSQL 16, 3NF).
-- Holds data that needs ACID guarantees: tenancy, subscriptions, identity,
-- fleet/vehicle master data, alerts, work orders, ML scores and audit.
-- High-volume telemetry lives in ClickHouse; live state in Redis.

CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS vector;

-- ---------------------------------------------------------------- platform
CREATE TABLE plan (
    plan_code               text PRIMARY KEY,
    name                    text          NOT NULL,
    price_per_vehicle_month numeric(10,2) NOT NULL CHECK (price_per_vehicle_month >= 0),
    api_rate_per_min        integer       NOT NULL CHECK (api_rate_per_min > 0),
    features                jsonb         NOT NULL DEFAULT '{}'
);

CREATE TABLE tenant (
    tenant_id   uuid PRIMARY KEY,
    slug        text        NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9-]{3,40}$'),
    name        text        NOT NULL,
    data_region text        NOT NULL DEFAULT 'ap-south-1',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE subscription (
    subscription_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       uuid    NOT NULL REFERENCES tenant ON DELETE CASCADE,
    plan_code       text    NOT NULL REFERENCES plan,
    status          text    NOT NULL CHECK (status IN ('TRIAL', 'ACTIVE', 'PAST_DUE', 'CANCELLED')),
    vehicle_quota   integer NOT NULL CHECK (vehicle_quota > 0),
    starts_on       date    NOT NULL,
    ends_on         date,
    CHECK (ends_on IS NULL OR ends_on > starts_on)
);
-- At most one live subscription per tenant.
CREATE UNIQUE INDEX subscription_one_active ON subscription (tenant_id) WHERE status IN ('TRIAL', 'ACTIVE', 'PAST_DUE');

CREATE TABLE role (
    role_code   text PRIMARY KEY,
    description text NOT NULL
);

CREATE TABLE app_user (
    user_id       uuid PRIMARY KEY,
    tenant_id     uuid REFERENCES tenant ON DELETE CASCADE, -- NULL = platform operator
    email         text        NOT NULL UNIQUE CHECK (email = lower(email)),
    full_name     text        NOT NULL,
    password_hash text        NOT NULL,
    status        text        NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'LOCKED', 'DISABLED')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz
);

CREATE TABLE user_role (
    user_id   uuid NOT NULL REFERENCES app_user ON DELETE CASCADE,
    role_code text NOT NULL REFERENCES role,
    PRIMARY KEY (user_id, role_code)
);

-- ---------------------------------------------------------- vehicle master
CREATE TABLE oem (
    oem_code        text PRIMARY KEY,
    name            text    NOT NULL,
    wmi             char(3) NOT NULL UNIQUE,
    adapter_version integer NOT NULL DEFAULT 1
);

CREATE TABLE vehicle_model (
    model_id    uuid PRIMARY KEY,
    oem_code    text NOT NULL REFERENCES oem,
    name        text NOT NULL,
    powertrain  text NOT NULL CHECK (powertrain IN ('ICE', 'EV', 'HEV')),
    body_type   text NOT NULL,
    battery_kwh numeric(6,1),
    tank_l      numeric(6,1),
    UNIQUE (oem_code, name)
);

CREATE TABLE depot (
    depot_id  uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenant ON DELETE CASCADE,
    name      text             NOT NULL,
    city      text             NOT NULL,
    lat       double precision NOT NULL CHECK (lat BETWEEN -90 AND 90),
    lon       double precision NOT NULL CHECK (lon BETWEEN -180 AND 180),
    UNIQUE (depot_id, tenant_id)
);

CREATE TABLE fleet (
    fleet_id  uuid PRIMARY KEY,
    tenant_id uuid NOT NULL,
    depot_id  uuid NOT NULL,
    name      text NOT NULL,
    UNIQUE (tenant_id, name),
    UNIQUE (fleet_id, tenant_id),
    FOREIGN KEY (depot_id, tenant_id) REFERENCES depot (depot_id, tenant_id)
);

-- tenant_id is functionally dependent on fleet_id (a deliberate, documented
-- denormalisation for row-level security and index locality). The composite
-- FK (fleet_id, tenant_id) makes an update anomaly impossible.
CREATE TABLE vehicle (
    vin           char(17) PRIMARY KEY CHECK (vin ~ '^[A-HJ-NPR-Z0-9]{17}$'),
    tenant_id     uuid     NOT NULL,
    fleet_id      uuid     NOT NULL,
    model_id      uuid     NOT NULL REFERENCES vehicle_model,
    model_year    smallint NOT NULL CHECK (model_year BETWEEN 1990 AND 2100),
    plate         text     NOT NULL,
    status        text     NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'IN_SERVICE', 'DECOMMISSIONED')),
    in_service_on date     NOT NULL,
    FOREIGN KEY (fleet_id, tenant_id) REFERENCES fleet (fleet_id, tenant_id)
);

CREATE TABLE driver (
    driver_id      uuid PRIMARY KEY,
    tenant_id      uuid        NOT NULL REFERENCES tenant ON DELETE CASCADE,
    full_name      text        NOT NULL,
    license_hash   text        NOT NULL,  -- SHA-256 for lookup; the number itself is encrypted
    license_no_enc bytea       NOT NULL,  -- pgp_sym_encrypt(AES-256) with a key from the secret store
    phone_enc      bytea,
    safety_opt_in  boolean     NOT NULL DEFAULT true,
    status         text        NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'INACTIVE', 'ERASED')),
    erased_at      timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (driver_id, tenant_id)
);

CREATE TABLE vehicle_assignment (
    assignment_id bigserial PRIMARY KEY,
    tenant_id     uuid        NOT NULL,
    vin           char(17)    NOT NULL REFERENCES vehicle ON DELETE CASCADE,
    driver_id     uuid        NOT NULL,
    valid_from    timestamptz NOT NULL,
    valid_to      timestamptz,
    CHECK (valid_to IS NULL OR valid_to > valid_from),
    FOREIGN KEY (driver_id, tenant_id) REFERENCES driver (driver_id, tenant_id)
);
CREATE UNIQUE INDEX vehicle_assignment_one_open ON vehicle_assignment (vin) WHERE valid_to IS NULL;
CREATE INDEX vehicle_assignment_driver ON vehicle_assignment (driver_id);

-- ------------------------------------------------------------ operations
CREATE TABLE dtc_code (
    code        text PRIMARY KEY CHECK (code ~ '^[PCBU][0-3][0-9A-F]{3}$'),
    description text NOT NULL,
    severity    text NOT NULL CHECK (severity IN ('CRITICAL', 'MAJOR', 'MINOR')),
    component   text NOT NULL
);

-- Trips: partitioned by month on start_ts (range) for pruning and cheap
-- retention (DROP PARTITION instead of DELETE). Locations are stored as
-- geohash-6 (~1 km) only: data minimisation for driver privacy.
CREATE TABLE trip (
    trip_id         uuid          NOT NULL,
    tenant_id       uuid          NOT NULL,
    vin             char(17)      NOT NULL,
    driver_id       uuid,
    start_ts        timestamptz   NOT NULL,
    end_ts          timestamptz   NOT NULL,
    distance_km     numeric(8,1)  NOT NULL CHECK (distance_km >= 0),
    max_speed_kmh   numeric(5,1)  NOT NULL,
    idle_s          integer       NOT NULL DEFAULT 0,
    harsh_brakes    integer       NOT NULL DEFAULT 0,
    harsh_accels    integer       NOT NULL DEFAULT 0,
    overspeed_s     integer       NOT NULL DEFAULT 0,
    energy_used_pct numeric(6,1)  NOT NULL DEFAULT 0,
    start_geohash   text          NOT NULL,
    end_geohash     text          NOT NULL,
    PRIMARY KEY (trip_id, start_ts),
    CHECK (end_ts >= start_ts)
) PARTITION BY RANGE (start_ts);

CREATE TABLE alert (
    alert_id        uuid PRIMARY KEY,
    tenant_id       uuid        NOT NULL,
    vin             char(17)    NOT NULL REFERENCES vehicle ON DELETE CASCADE,
    alert_type      text        NOT NULL,
    severity        text        NOT NULL CHECK (severity IN ('CRITICAL', 'HIGH', 'MEDIUM', 'LOW')),
    title           text        NOT NULL,
    status          text        NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN', 'ACKNOWLEDGED', 'RESOLVED')),
    ts              timestamptz NOT NULL,
    detected_at     timestamptz NOT NULL,
    lat             double precision,
    lon             double precision,
    details         jsonb       NOT NULL DEFAULT '{}',
    acknowledged_by uuid REFERENCES app_user,
    acknowledged_at timestamptz,
    resolved_at     timestamptz
);

CREATE TABLE work_order (
    work_order_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL,
    vin           char(17)    NOT NULL REFERENCES vehicle ON DELETE CASCADE,
    source        text        NOT NULL CHECK (source IN ('PREDICTION', 'ALERT', 'MANUAL', 'COPILOT')),
    component     text,
    priority      text        NOT NULL CHECK (priority IN ('P1', 'P2', 'P3')),
    status        text        NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN', 'SCHEDULED', 'IN_PROGRESS', 'DONE', 'CANCELLED')),
    due_on        date,
    est_cost_usd  numeric(10,2),
    notes         text,
    created_by    uuid REFERENCES app_user,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    version       integer     NOT NULL DEFAULT 1  -- optimistic concurrency
);

-- Ground-truth maintenance history: labels for the predictive model.
CREATE TABLE service_event (
    service_event_id uuid PRIMARY KEY,
    tenant_id        uuid          NOT NULL,
    vin              char(17)      NOT NULL REFERENCES vehicle ON DELETE CASCADE,
    event_type       text          NOT NULL CHECK (event_type IN ('BREAKDOWN', 'REPAIR', 'PLANNED_SERVICE')),
    component        text          NOT NULL,
    occurred_at      timestamptz   NOT NULL,
    cost_usd         numeric(10,2) NOT NULL DEFAULT 0,
    work_order_id    uuid REFERENCES work_order,
    notes            text
);

-- ------------------------------------------------------------------- ML
CREATE TABLE model_version (
    model_version text PRIMARY KEY,
    trained_at    timestamptz NOT NULL DEFAULT now(),
    algorithm     text        NOT NULL,
    metrics       jsonb       NOT NULL,
    features      jsonb       NOT NULL,
    is_active     boolean     NOT NULL DEFAULT false
);
CREATE UNIQUE INDEX model_version_one_active ON model_version (is_active) WHERE is_active;

-- Append-only score history (one row per vehicle per scoring day).
CREATE TABLE risk_score (
    vin                   char(17)      NOT NULL REFERENCES vehicle ON DELETE CASCADE,
    scored_on             date          NOT NULL,
    model_version         text          NOT NULL REFERENCES model_version,
    tenant_id             uuid          NOT NULL,
    risk_7d               real          NOT NULL CHECK (risk_7d BETWEEN 0 AND 1),
    top_component         text,
    top_factors           jsonb         NOT NULL DEFAULT '[]',
    est_breakdown_cost    numeric(10,2) NOT NULL DEFAULT 0,
    expected_savings_usd  numeric(10,2) NOT NULL DEFAULT 0,
    PRIMARY KEY (vin, scored_on, model_version)
);

-- Knowledge base for retrieval (vector store): DTC guides, repair notes.
CREATE TABLE fault_knowledge (
    kb_id     uuid PRIMARY KEY,
    dtc_code  text REFERENCES dtc_code,
    component text NOT NULL,
    title     text NOT NULL,
    content   text NOT NULL,
    source    text NOT NULL,
    embedding vector(256)
);

-- -------------------------------------------------------- compliance / AI
CREATE TABLE audit_log (
    audit_id      bigserial PRIMARY KEY,
    ts            timestamptz NOT NULL DEFAULT now(),
    tenant_id     uuid,
    actor_id      uuid,
    actor_type    text NOT NULL CHECK (actor_type IN ('USER', 'AGENT', 'SYSTEM', 'SERVICE')),
    action        text NOT NULL,
    resource_type text NOT NULL,
    resource_id   text,
    outcome       text NOT NULL CHECK (outcome IN ('ALLOW', 'DENY', 'ERROR')),
    ip            inet,
    request_id    text,
    details       jsonb NOT NULL DEFAULT '{}',
    prev_hash     bytea,
    hash          bytea NOT NULL
);

CREATE TABLE erasure_request (
    request_id   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid        NOT NULL,
    subject_type text        NOT NULL CHECK (subject_type IN ('DRIVER')),
    subject_id   uuid        NOT NULL,
    requested_by uuid REFERENCES app_user,
    reason       text        NOT NULL,
    status       text        NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'COMPLETED', 'REJECTED')),
    requested_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    report       jsonb       NOT NULL DEFAULT '{}'
);

CREATE TABLE agent_action (
    action_id       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL,
    user_id         uuid        NOT NULL REFERENCES app_user,
    conversation_id uuid        NOT NULL,
    tool            text        NOT NULL,
    arguments       jsonb       NOT NULL,
    rationale       text,
    status          text        NOT NULL DEFAULT 'PROPOSED' CHECK (status IN ('PROPOSED', 'APPROVED', 'REJECTED', 'EXECUTED', 'FAILED')),
    result          jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    decided_by      uuid REFERENCES app_user,
    decided_at      timestamptz
);
