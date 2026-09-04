-- name: GetContextProjectionNodeForSource :one
SELECT *
FROM context_projection_nodes
WHERE session_id = ?
  AND batch_key = ?
  AND source_hash = ?
  AND algorithm_version = ?
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: GetActiveContextProjectionNodeForSource :one
SELECT *
FROM context_projection_nodes
WHERE session_id = ?
  AND batch_key = ?
  AND source_hash = ?
  AND algorithm_version = ?
  AND state = 'active'
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: GetContextProjectionNode :one
SELECT *
FROM context_projection_nodes
WHERE session_id = ?
  AND batch_key = ?
  AND source_hash = ?
  AND algorithm_version = ?
  AND configuration_id = ?
LIMIT 1;

-- name: CreateContextProjectionClaim :one
INSERT INTO context_projection_nodes (
    id,
    session_id,
    batch_key,
    source_hash,
    algorithm_version,
    configuration_id,
    state,
    first_message_id,
    last_message_id,
    raw_chars,
    lease_expires_at,
    claim_token,
    attempt_count,
    created_at,
    updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?, ?, ?, 1, ?, ?
)
ON CONFLICT (session_id, batch_key, source_hash, algorithm_version, configuration_id) DO NOTHING
RETURNING *;

-- name: ReclaimContextProjectionClaim :one
UPDATE context_projection_nodes
SET state = 'pending',
    lease_expires_at = sqlc.arg(lease_expires_at),
    claim_token = sqlc.arg(claim_token),
    attempt_count = attempt_count + 1,
    retry_after = NULL,
    failure = '',
    summarizer_provider = '',
    summarizer_model = '',
    prompt_tokens = 0,
    completion_tokens = 0,
    cost = 0,
    updated_at = sqlc.arg(updated_at)
WHERE session_id = sqlc.arg(session_id)
  AND batch_key = sqlc.arg(batch_key)
  AND source_hash = sqlc.arg(source_hash)
  AND algorithm_version = sqlc.arg(algorithm_version)
  AND configuration_id = sqlc.arg(configuration_id)
  AND (
      state = 'stale'
      OR
      (state = 'pending' AND lease_expires_at <= sqlc.arg(now))
      OR
      (state = 'failed' AND retry_after IS NOT NULL AND retry_after <= sqlc.arg(now))
  )
RETURNING *;

-- name: GetContextProjectionClaim :one
SELECT *
FROM context_projection_nodes
WHERE id = ?
  AND session_id = ?
  AND algorithm_version = ?
  AND state = 'pending'
  AND claim_token = ?
LIMIT 1;

-- name: ListCanonicalContextProjectionMessages :many
SELECT m.*
FROM messages AS m
WHERE m.session_id = ?
ORDER BY m.created_at ASC, m.rowid ASC;

-- name: ListActiveContextProjectionNodes :many
SELECT *
FROM context_projection_nodes
WHERE session_id = ?
  AND algorithm_version = ?
  AND state = 'active'
ORDER BY created_at ASC, id ASC;

-- name: ListAllActiveContextProjectionNodesBySession :many
SELECT *
FROM context_projection_nodes
WHERE session_id = ?
  AND state = 'active'
ORDER BY created_at ASC, id ASC;

-- name: ListActiveContextProjectionNodesBySourceMessage :many
SELECT DISTINCT n.*
FROM context_projection_nodes AS n
JOIN context_projection_sources AS s
  ON s.node_id = n.id
 AND s.session_id = n.session_id
WHERE n.state = 'active'
  AND s.session_id = sqlc.arg(session_id)
  AND s.source_message_id = sqlc.arg(source_message_id)
ORDER BY n.created_at ASC, n.id ASC;

-- name: ListContextProjectionSourcesByNode :many
SELECT *
FROM context_projection_sources
WHERE node_id = ?
ORDER BY ordinal ASC;

-- name: ListContextProjectionItemsByNode :many
SELECT *
FROM context_projection_items
WHERE node_id = ?
ORDER BY ordinal ASC;

-- name: GetActiveContextProjectionItemByRef :one
SELECT
    i.id,
    i.node_id,
    i.session_id,
    i.source_message_id,
    i.source_part_ordinal,
    i.tool_call_id,
    i.tool_name,
    i.description,
    i.ref_number,
    i.source_hash,
    i.ordinal,
    i.algorithm_version
FROM context_projection_items AS i
JOIN context_projection_nodes AS n
  ON n.id = i.node_id
 AND n.session_id = i.session_id
 AND n.algorithm_version = i.algorithm_version
WHERE i.session_id = ?
  AND i.ref_number = ?
  AND n.state = 'active'
LIMIT 1;

-- name: ReserveContextProjectionRefs :one
UPDATE context_projection_ref_counter
SET next_ref_number = next_ref_number + sqlc.arg(count)
WHERE singleton = 1
  AND sqlc.arg(count) > 0
  AND next_ref_number <= 9223372036854775807 - sqlc.arg(count)
RETURNING next_ref_number - sqlc.arg(count);

-- name: CreateContextProjectionSource :exec
INSERT INTO context_projection_sources (
    node_id,
    session_id,
    source_message_id,
    source_part_ordinal,
    part_kind,
    source_hash,
    ordinal
) VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: CreateContextProjectionItem :exec
INSERT INTO context_projection_items (
    id,
    node_id,
    session_id,
    source_message_id,
    source_part_ordinal,
    tool_call_id,
    tool_name,
    description,
    ref_number,
    source_hash,
    ordinal,
    algorithm_version
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ActivateContextProjectionClaim :execrows
UPDATE context_projection_nodes
SET state = 'active',
    summary = sqlc.arg(summary),
    projected_chars = sqlc.arg(projected_chars),
    lease_expires_at = NULL,
    claim_token = NULL,
    retry_after = NULL,
    failure = '',
    summarizer_provider = sqlc.arg(summarizer_provider),
    summarizer_model = sqlc.arg(summarizer_model),
    prompt_tokens = sqlc.arg(prompt_tokens),
    completion_tokens = sqlc.arg(completion_tokens),
    cost = sqlc.arg(cost),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id)
  AND session_id = sqlc.arg(session_id)
  AND algorithm_version = sqlc.arg(algorithm_version)
  AND state = 'pending'
  AND claim_token = sqlc.arg(claim_token);

-- name: SkipContextProjectionClaim :execrows
UPDATE context_projection_nodes
SET state = 'skipped',
    lease_expires_at = NULL,
    claim_token = NULL,
    retry_after = NULL,
    failure = sqlc.arg(failure),
    summarizer_provider = sqlc.arg(summarizer_provider),
    summarizer_model = sqlc.arg(summarizer_model),
    prompt_tokens = sqlc.arg(prompt_tokens),
    completion_tokens = sqlc.arg(completion_tokens),
    cost = sqlc.arg(cost),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id)
  AND session_id = sqlc.arg(session_id)
  AND algorithm_version = sqlc.arg(algorithm_version)
  AND state = 'pending'
  AND claim_token = sqlc.arg(claim_token);

-- name: FailContextProjectionClaim :execrows
UPDATE context_projection_nodes
SET state = 'failed',
    lease_expires_at = NULL,
    claim_token = NULL,
    retry_after = sqlc.arg(retry_after),
    failure = sqlc.arg(failure),
    summarizer_provider = sqlc.arg(summarizer_provider),
    summarizer_model = sqlc.arg(summarizer_model),
    prompt_tokens = sqlc.arg(prompt_tokens),
    completion_tokens = sqlc.arg(completion_tokens),
    cost = sqlc.arg(cost),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id)
  AND session_id = sqlc.arg(session_id)
  AND algorithm_version = sqlc.arg(algorithm_version)
  AND state = 'pending'
  AND claim_token = sqlc.arg(claim_token);

-- name: StaleContextProjectionClaim :execrows
UPDATE context_projection_nodes
SET state = 'stale',
    lease_expires_at = NULL,
    claim_token = NULL,
    retry_after = NULL,
    failure = sqlc.arg(failure),
    summarizer_provider = sqlc.arg(summarizer_provider),
    summarizer_model = sqlc.arg(summarizer_model),
    prompt_tokens = sqlc.arg(prompt_tokens),
    completion_tokens = sqlc.arg(completion_tokens),
    cost = sqlc.arg(cost),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id)
  AND session_id = sqlc.arg(session_id)
  AND algorithm_version = sqlc.arg(algorithm_version)
  AND state = 'pending'
  AND claim_token = sqlc.arg(claim_token);

-- name: StaleActiveContextProjectionNode :execrows
UPDATE context_projection_nodes
SET state = 'stale',
    failure = sqlc.arg(failure),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id)
  AND session_id = sqlc.arg(session_id)
  AND algorithm_version = sqlc.arg(algorithm_version)
  AND state = 'active';

-- name: ListConflictingActiveContextProjectionNodes :many
SELECT DISTINCT n.*
FROM context_projection_nodes AS n
JOIN context_projection_items AS i
  ON i.node_id = n.id
 AND i.session_id = n.session_id
 AND i.algorithm_version = n.algorithm_version
WHERE n.session_id = sqlc.arg(session_id)
  AND n.algorithm_version = sqlc.arg(algorithm_version)
  AND n.state = 'active'
  AND i.source_message_id = sqlc.arg(source_message_id)
  AND i.source_part_ordinal = sqlc.arg(source_part_ordinal)
ORDER BY n.created_at ASC, n.id ASC;

-- name: DeleteContextProjectionSourcesByNode :exec
DELETE FROM context_projection_sources
WHERE node_id = ?;

-- name: DeleteContextProjectionItemsByNode :exec
DELETE FROM context_projection_items
WHERE node_id = ?;

-- name: CountContextProjectionNodesBySession :one
SELECT count(*)
FROM context_projection_nodes
WHERE session_id = ?;

-- name: CountContextProjectionSourcesBySession :one
SELECT count(*)
FROM context_projection_sources
WHERE session_id = ?;

-- name: CountContextProjectionUsageAttemptsBySession :one
SELECT count(*)
FROM context_projection_usage_attempts
WHERE session_id = ?;

-- name: CountContextProjectionItemsBySession :one
SELECT count(*)
FROM context_projection_items
WHERE session_id = ?;

-- name: CreateContextProjectionUsageAttempt :exec
INSERT INTO context_projection_usage_attempts (
    node_id,
    session_id,
    attempt_number,
    summarizer_provider,
    summarizer_model,
    prompt_tokens,
    completion_tokens,
    cost,
    created_at,
    updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: AccountContextProjectionUsageAttempt :execrows
UPDATE context_projection_usage_attempts
SET accounted = 1,
    updated_at = sqlc.arg(updated_at)
WHERE node_id = sqlc.arg(node_id)
  AND attempt_number = sqlc.arg(attempt_number)
  AND session_id = sqlc.arg(session_id)
  AND accounted = 0;

-- name: ListUnaccountedContextProjectionUsage :many
SELECT *
FROM context_projection_usage_attempts
WHERE session_id = ?
  AND accounted = 0
ORDER BY created_at ASC, node_id ASC, attempt_number ASC;

-- name: GetContextProjectionRefCounter :one
SELECT next_ref_number
FROM context_projection_ref_counter
WHERE singleton = 1;
