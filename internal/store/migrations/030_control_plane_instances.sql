CREATE TABLE control_plane_instances (
    instance_id TEXT PRIMARY KEY,
    binary_version TEXT NOT NULL,
    protocol_min INTEGER NOT NULL CHECK (protocol_min > 0),
    protocol_max INTEGER NOT NULL CHECK (protocol_max >= protocol_min),
    started_at TIMESTAMPTZ NOT NULL,
    heartbeat_at TIMESTAMPTZ NOT NULL,
    draining BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE INDEX control_plane_instances_heartbeat_idx
    ON control_plane_instances(heartbeat_at);
