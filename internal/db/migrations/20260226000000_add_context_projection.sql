-- +goose Up
-- +goose StatementBegin
CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_session_id_id
    ON messages (session_id, id);

CREATE TABLE context_projection_nodes (
    id TEXT PRIMARY KEY CHECK (id != ''),
    session_id TEXT NOT NULL CHECK (session_id != ''),
    batch_key TEXT NOT NULL CHECK (length(batch_key) = 64),
    source_hash TEXT NOT NULL CHECK (length(source_hash) = 64),
    algorithm_version INTEGER NOT NULL CHECK (algorithm_version > 0),
    configuration_id TEXT NOT NULL CHECK (length(configuration_id) = 64),
    state TEXT NOT NULL CHECK (state IN ('pending', 'active', 'skipped', 'failed', 'stale')),
    summary TEXT NOT NULL DEFAULT '' CHECK (length(summary) <= 4000),
    first_message_id TEXT NOT NULL CHECK (first_message_id != ''),
    last_message_id TEXT NOT NULL CHECK (last_message_id != ''),
    raw_chars INTEGER NOT NULL CHECK (raw_chars >= 0),
    projected_chars INTEGER NOT NULL DEFAULT 0 CHECK (projected_chars >= 0),
    lease_expires_at INTEGER,
    claim_token TEXT,
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    retry_after INTEGER,
    failure TEXT NOT NULL DEFAULT '' CHECK (length(failure) <= 1000),
    summarizer_provider TEXT NOT NULL DEFAULT '',
    summarizer_model TEXT NOT NULL DEFAULT '',
    prompt_tokens INTEGER NOT NULL DEFAULT 0 CHECK (prompt_tokens >= 0),
    completion_tokens INTEGER NOT NULL DEFAULT 0 CHECK (completion_tokens >= 0),
    cost REAL NOT NULL DEFAULT 0 CHECK (cost >= 0),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY (session_id) REFERENCES sessions (id) ON DELETE CASCADE,
    UNIQUE (session_id, id),
    UNIQUE (session_id, id, algorithm_version),
    UNIQUE (session_id, batch_key, source_hash, algorithm_version, configuration_id),
    CHECK (
        (state = 'pending' AND claim_token IS NOT NULL AND claim_token != '' AND lease_expires_at IS NOT NULL)
        OR
        (state != 'pending' AND claim_token IS NULL AND lease_expires_at IS NULL)
    ),
    CHECK (state != 'active' OR summary != '')
);

CREATE TABLE context_projection_usage_attempts (
    node_id TEXT NOT NULL CHECK (node_id != ''),
    session_id TEXT NOT NULL CHECK (session_id != ''),
    attempt_number INTEGER NOT NULL CHECK (attempt_number > 0),
    summarizer_provider TEXT NOT NULL DEFAULT '',
    summarizer_model TEXT NOT NULL DEFAULT '',
    prompt_tokens INTEGER NOT NULL DEFAULT 0 CHECK (prompt_tokens >= 0),
    completion_tokens INTEGER NOT NULL DEFAULT 0 CHECK (completion_tokens >= 0),
    cost REAL NOT NULL DEFAULT 0 CHECK (cost >= 0),
    accounted INTEGER NOT NULL DEFAULT 0 CHECK (accounted IN (0, 1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY (session_id) REFERENCES sessions (id) ON DELETE CASCADE,
    FOREIGN KEY (session_id, node_id)
        REFERENCES context_projection_nodes (session_id, id) ON DELETE CASCADE,
    PRIMARY KEY (node_id, attempt_number)
);

CREATE TABLE context_projection_sources (
    node_id TEXT NOT NULL CHECK (node_id != ''),
    session_id TEXT NOT NULL CHECK (session_id != ''),
    source_message_id TEXT NOT NULL CHECK (source_message_id != ''),
    source_part_ordinal INTEGER NOT NULL CHECK (source_part_ordinal >= 0),
    part_kind TEXT NOT NULL CHECK (part_kind != ''),
    source_hash TEXT NOT NULL CHECK (length(source_hash) = 64),
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    FOREIGN KEY (session_id) REFERENCES sessions (id) ON DELETE CASCADE,
    FOREIGN KEY (session_id, node_id)
        REFERENCES context_projection_nodes (session_id, id) ON DELETE CASCADE,
    FOREIGN KEY (session_id, source_message_id)
        REFERENCES messages (session_id, id),
    UNIQUE (node_id, source_message_id, source_part_ordinal),
    UNIQUE (node_id, ordinal)
);

CREATE TABLE context_projection_items (
    id TEXT PRIMARY KEY CHECK (id != ''),
    node_id TEXT NOT NULL CHECK (node_id != ''),
    session_id TEXT NOT NULL CHECK (session_id != ''),
    source_message_id TEXT NOT NULL CHECK (source_message_id != ''),
    source_part_ordinal INTEGER NOT NULL CHECK (source_part_ordinal >= 0),
    tool_call_id TEXT NOT NULL CHECK (tool_call_id != ''),
    tool_name TEXT NOT NULL CHECK (tool_name != ''),
    description TEXT NOT NULL CHECK (description != '' AND length(description) <= 500),
    ref_number INTEGER NOT NULL CHECK (ref_number > 0),
    source_hash TEXT NOT NULL CHECK (length(source_hash) = 64),
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    algorithm_version INTEGER NOT NULL CHECK (algorithm_version > 0),
    FOREIGN KEY (session_id) REFERENCES sessions (id) ON DELETE CASCADE,
    FOREIGN KEY (session_id, node_id, algorithm_version)
        REFERENCES context_projection_nodes (session_id, id, algorithm_version) ON DELETE CASCADE,
    FOREIGN KEY (session_id, source_message_id)
        REFERENCES messages (session_id, id),
    FOREIGN KEY (node_id, source_message_id, source_part_ordinal)
        REFERENCES context_projection_sources (node_id, source_message_id, source_part_ordinal) ON DELETE CASCADE,
    UNIQUE (node_id, source_message_id, source_part_ordinal),
    UNIQUE (ref_number),
    UNIQUE (session_id, source_message_id, source_part_ordinal, algorithm_version)
);

-- This high-water mark is never derived from active items or decremented during
-- normal runtime, so deleted and replaced projections cannot reuse a visible ref.
CREATE TABLE context_projection_ref_counter (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    next_ref_number INTEGER NOT NULL CHECK (next_ref_number > 0)
);

INSERT INTO context_projection_ref_counter (singleton, next_ref_number)
VALUES (1, 1);

CREATE INDEX idx_context_projection_nodes_active
    ON context_projection_nodes (session_id, algorithm_version, state);
CREATE INDEX idx_context_projection_nodes_candidate
    ON context_projection_nodes (
        session_id, batch_key, source_hash, algorithm_version, configuration_id
    );
CREATE INDEX idx_context_projection_usage_unaccounted
    ON context_projection_usage_attempts (session_id, accounted, created_at);
CREATE INDEX idx_context_projection_items_session_ref
    ON context_projection_items (session_id, ref_number);
CREATE INDEX idx_context_projection_items_node_ordinal
    ON context_projection_items (node_id, ordinal);
CREATE INDEX idx_context_projection_sources_message
    ON context_projection_sources (session_id, source_message_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- An explicit feature rollback drops the feature tables and therefore does not
-- attempt to preserve refs for a later re-up migration.
DROP INDEX IF EXISTS idx_context_projection_sources_message;
DROP INDEX IF EXISTS idx_context_projection_items_node_ordinal;
DROP INDEX IF EXISTS idx_context_projection_items_session_ref;
DROP INDEX IF EXISTS idx_context_projection_usage_unaccounted;
DROP INDEX IF EXISTS idx_context_projection_nodes_candidate;
DROP INDEX IF EXISTS idx_context_projection_nodes_active;
DROP TABLE IF EXISTS context_projection_ref_counter;
DROP TABLE IF EXISTS context_projection_items;
DROP TABLE IF EXISTS context_projection_sources;
DROP TABLE IF EXISTS context_projection_usage_attempts;
DROP TABLE IF EXISTS context_projection_nodes;
DROP INDEX IF EXISTS idx_messages_session_id_id;
-- +goose StatementEnd
