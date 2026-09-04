package condense

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestMigrationCreatesProjectionSchemaAndCascades(t *testing.T) {
	t.Parallel()

	env := newTestEnvironment(t, nil, enabledOptions())
	batch := env.createBatch(t, strings.Repeat("canonical", 100))
	candidate := scanCandidates(batch)[0]
	claimed, ok, err := env.store.claim(t.Context(), candidate, time.Unix(100, 0), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	summary, err := validateSummary(Summary{
		Text: "summary", Items: []SummaryItem{{Ordinal: 0, Description: "description"}},
	}, 1)
	require.NoError(t, err)
	activated, err := env.store.activate(t.Context(), claimed, summary, time.Unix(101, 0))
	require.NoError(t, err)
	require.True(t, activated)

	nodes, err := env.queries.CountContextProjectionNodesBySession(t.Context(), env.sessionID)
	require.NoError(t, err)
	sources, err := env.queries.CountContextProjectionSourcesBySession(t.Context(), env.sessionID)
	require.NoError(t, err)
	items, err := env.queries.CountContextProjectionItemsBySession(t.Context(), env.sessionID)
	require.NoError(t, err)
	require.Equal(t, int64(1), nodes)
	require.Positive(t, sources)
	require.Equal(t, int64(1), items)

	require.NoError(t, env.queries.DeleteSession(t.Context(), env.sessionID))
	nodes, err = env.queries.CountContextProjectionNodesBySession(t.Context(), env.sessionID)
	require.NoError(t, err)
	sources, err = env.queries.CountContextProjectionSourcesBySession(t.Context(), env.sessionID)
	require.NoError(t, err)
	items, err = env.queries.CountContextProjectionItemsBySession(t.Context(), env.sessionID)
	require.NoError(t, err)
	require.Zero(t, nodes)
	require.Zero(t, sources)
	require.Zero(t, items)
}

func TestStoreClaimLeaseRetryAndFence(t *testing.T) {
	t.Parallel()

	env := newTestEnvironment(t, nil, enabledOptions())
	candidate := scanCandidates(env.createBatch(t, strings.Repeat("x", 500)))[0]
	now := time.Unix(1_000, 0)
	first, ok, err := env.store.claim(t.Context(), candidate, now, time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(1), first.attemptCount)

	_, ok, err = env.store.claim(t.Context(), candidate, now.Add(30*time.Second), time.Minute)
	require.NoError(t, err)
	require.False(t, ok)

	second, ok, err := env.store.claim(t.Context(), candidate, now.Add(2*time.Minute), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(2), second.attemptCount)
	require.NotEqual(t, first.token, second.token)
	require.Error(t, env.store.skip(t.Context(), first, "stale owner", now.Add(2*time.Minute)))

	retryAt := now.Add(10 * time.Minute)
	require.NoError(t, env.store.fail(t.Context(), second, "transient", &retryAt, Usage{}, now.Add(2*time.Minute)))
	_, ok, err = env.store.claim(t.Context(), candidate, retryAt.Add(-time.Second), time.Minute)
	require.NoError(t, err)
	require.False(t, ok)
	third, ok, err := env.store.claim(t.Context(), candidate, retryAt, time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(3), third.attemptCount)
}

func TestStoreRefAllocationIsPermanentAndActivationAtomic(t *testing.T) {
	t.Parallel()

	env := newTestEnvironment(t, nil, enabledOptions())
	activate := func(content string) int64 {
		batch := env.createBatch(t, content)
		candidate := scanCandidates(batch)[0]
		claimed, ok, err := env.store.claim(t.Context(), candidate, time.Now(), time.Minute)
		require.NoError(t, err)
		require.True(t, ok)
		activated, err := env.store.activate(t.Context(), claimed, Summary{
			Text: "summary", Items: []SummaryItem{{Ordinal: 0, Description: "description"}},
		}, time.Now())
		require.NoError(t, err)
		require.True(t, activated)
		items, err := env.queries.ListContextProjectionItemsByNode(t.Context(), claimed.nodeID)
		require.NoError(t, err)
		require.Len(t, items, 1)
		return items[0].RefNumber
	}

	first := activate(strings.Repeat("a", 1_000))
	active, err := env.store.active(t.Context(), env.sessionID)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.NoError(t, env.queries.DeleteContextProjectionItemsByNode(t.Context(), active[0].node.ID))
	second := activate(strings.Repeat("b", 1_000))
	require.Greater(t, second, first)

	// A forced transaction rollback cannot expose either active state or items.
	candidate := scanCandidates(env.createBatch(t, strings.Repeat("c", 1_000)))[0]
	claimed, ok, err := env.store.claim(t.Context(), candidate, time.Now(), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	tx, err := env.store.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	qtx := env.queries.WithTx(tx)
	require.NoError(t, qtx.CreateContextProjectionSource(t.Context(), db.CreateContextProjectionSourceParams{
		NodeID: claimed.nodeID, SessionID: env.sessionID, SourceMessageID: candidate.Items[0].MessageID,
		SourcePartOrdinal: int64(candidate.Items[0].PartOrdinal), PartKind: "tool_result",
		SourceHash: candidate.Items[0].SourceHash, Ordinal: 0,
	}))
	require.NoError(t, tx.Rollback())
	sources, err := env.queries.ListContextProjectionSourcesByNode(t.Context(), claimed.nodeID)
	require.NoError(t, err)
	require.Empty(t, sources)
	node, err := env.store.node(t.Context(), candidate)
	require.NoError(t, err)
	require.Equal(t, "pending", node.State)
}

func TestStoreConstraintsRejectCrossSessionAndAlgorithmOverlap(t *testing.T) {
	t.Parallel()

	env := newTestEnvironment(t, nil, enabledOptions())
	candidate := scanCandidates(env.createBatch(t, strings.Repeat("x", 1_000)))[0]
	claimed, ok, err := env.store.claim(t.Context(), candidate, time.Now(), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	_, err = env.store.db.ExecContext(t.Context(), `
		INSERT INTO context_projection_sources (
			node_id, session_id, source_message_id, source_part_ordinal,
			part_kind, source_hash, ordinal
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		claimed.nodeID, "another-session", candidate.Items[0].MessageID,
		candidate.Items[0].PartOrdinal, "tool_result", candidate.Items[0].SourceHash, 0,
	)
	require.Error(t, err)

	_, err = env.store.db.ExecContext(t.Context(), `
		INSERT INTO context_projection_ref_counter (singleton, next_ref_number) VALUES (2, 1)`)
	require.Error(t, err)
}

func TestStoreDeleteMessageStalesWholeProjection(t *testing.T) {
	t.Parallel()

	for _, source := range []string{"assistant", "tool"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()

			env := newTestEnvironment(t, nil, enabledOptions())
			batch := env.createBatch(t, strings.Repeat("canonical", 100))
			candidate := scanCandidates(batch)[0]
			claimed, ok, err := env.store.claim(t.Context(), candidate, time.Now(), time.Minute)
			require.NoError(t, err)
			require.True(t, ok)
			activated, err := env.store.activate(t.Context(), claimed, Summary{
				Text: "summary", Items: []SummaryItem{{Ordinal: 0, Description: "description"}},
			}, time.Now())
			require.NoError(t, err)
			require.True(t, activated)
			items, err := env.queries.ListContextProjectionItemsByNode(t.Context(), claimed.nodeID)
			require.NoError(t, err)
			require.Len(t, items, 1)
			refNumber := items[0].RefNumber

			messageID := batch[0].ID
			if source == "tool" {
				messageID = batch[1].ID
			}
			require.NoError(t, env.store.DeleteMessage(t.Context(), messageID))
			_, err = env.messages.Get(t.Context(), messageID)
			require.ErrorIs(t, err, sql.ErrNoRows)

			node, err := env.store.node(t.Context(), candidate)
			require.NoError(t, err)
			require.Equal(t, "stale", node.State)
			sources, err := env.queries.ListContextProjectionSourcesByNode(t.Context(), claimed.nodeID)
			require.NoError(t, err)
			require.Empty(t, sources)
			items, err = env.queries.ListContextProjectionItemsByNode(t.Context(), claimed.nodeID)
			require.NoError(t, err)
			require.Empty(t, items)
			_, err = env.queries.GetActiveContextProjectionItemByRef(t.Context(), db.GetActiveContextProjectionItemByRefParams{
				SessionID: env.sessionID, RefNumber: refNumber,
			})
			require.ErrorIs(t, err, sql.ErrNoRows)
		})
	}
}

func TestMessageServiceDeleteUsesProjectionLifecycle(t *testing.T) {
	t.Parallel()

	env := newTestEnvironment(t, nil, enabledOptions())
	batch := env.createBatch(t, strings.Repeat("canonical", 100))
	candidate := scanCandidates(batch)[0]
	claimed, ok, err := env.store.claim(t.Context(), candidate, time.Now(), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	activated, err := env.store.activate(t.Context(), claimed, Summary{
		Text: "summary", Items: []SummaryItem{{Ordinal: 0, Description: "description"}},
	}, time.Now())
	require.NoError(t, err)
	require.True(t, activated)

	messages := message.NewService(env.queries, message.WithDebounce(time.Hour), message.WithMessageDeleter(env.store))
	pending, err := messages.Get(t.Context(), batch[1].ID)
	require.NoError(t, err)
	result := pending.Parts[0].(message.ToolResult)
	result.Content = "buffered update"
	pending.Parts[0] = result
	require.NoError(t, messages.Update(t.Context(), pending))

	require.NoError(t, messages.Delete(t.Context(), batch[1].ID))
	require.NoError(t, messages.FlushAll(t.Context()))
	_, err = messages.Get(t.Context(), batch[1].ID)
	require.ErrorIs(t, err, sql.ErrNoRows)
	node, err := env.store.node(t.Context(), candidate)
	require.NoError(t, err)
	require.Equal(t, "stale", node.State)
}

func TestStoreDeleteMessageRollsBackProjectionInvalidation(t *testing.T) {
	t.Parallel()

	env := newTestEnvironment(t, nil, enabledOptions())
	batch := env.createBatch(t, strings.Repeat("canonical", 100))
	candidate := scanCandidates(batch)[0]
	claimed, ok, err := env.store.claim(t.Context(), candidate, time.Now(), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	activated, err := env.store.activate(t.Context(), claimed, Summary{
		Text: "summary", Items: []SummaryItem{{Ordinal: 0, Description: "description"}},
	}, time.Now())
	require.NoError(t, err)
	require.True(t, activated)

	_, err = env.store.db.ExecContext(t.Context(), `
		CREATE TRIGGER fail_projection_source_delete
		BEFORE DELETE ON messages
		WHEN OLD.id = '`+batch[1].ID+`'
		BEGIN
			SELECT RAISE(ABORT, 'forced message delete failure');
		END`)
	require.NoError(t, err)
	err = env.store.DeleteMessage(t.Context(), batch[1].ID)
	require.ErrorContains(t, err, "forced message delete failure")

	_, err = env.messages.Get(t.Context(), batch[1].ID)
	require.NoError(t, err)
	node, err := env.store.node(t.Context(), candidate)
	require.NoError(t, err)
	require.Equal(t, "active", node.State)
	sources, err := env.queries.ListContextProjectionSourcesByNode(t.Context(), claimed.nodeID)
	require.NoError(t, err)
	require.NotEmpty(t, sources)
	items, err := env.queries.ListContextProjectionItemsByNode(t.Context(), claimed.nodeID)
	require.NoError(t, err)
	require.NotEmpty(t, items)
}

func TestStoreDeleteMessageKeepsRefsMonotonic(t *testing.T) {
	t.Parallel()

	env := newTestEnvironment(t, nil, enabledOptions())
	activate := func(content string) ([]message.Message, int64) {
		batch := env.createBatch(t, content)
		candidate := scanCandidates(batch)[0]
		claimed, ok, err := env.store.claim(t.Context(), candidate, time.Now(), time.Minute)
		require.NoError(t, err)
		require.True(t, ok)
		activated, err := env.store.activate(t.Context(), claimed, Summary{
			Text: "summary", Items: []SummaryItem{{Ordinal: 0, Description: "description"}},
		}, time.Now())
		require.NoError(t, err)
		require.True(t, activated)
		items, err := env.queries.ListContextProjectionItemsByNode(t.Context(), claimed.nodeID)
		require.NoError(t, err)
		require.Len(t, items, 1)
		return batch, items[0].RefNumber
	}

	firstBatch, firstRef := activate(strings.Repeat("first", 300))
	require.NoError(t, env.store.DeleteMessage(t.Context(), firstBatch[1].ID))
	_, secondRef := activate(strings.Repeat("second", 300))
	require.Greater(t, secondRef, firstRef)
}

func TestStoreForkKeepsOriginalProjectionAndBuildsForkProjection(t *testing.T) {
	t.Parallel()

	env := newTestEnvironment(t, nil, enabledOptions())
	originalBatch := env.createBatch(t, strings.Repeat("canonical", 100))
	originalCandidate := scanCandidates(originalBatch)[0]
	originalClaim, ok, err := env.store.claim(t.Context(), originalCandidate, time.Now(), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	activated, err := env.store.activate(t.Context(), originalClaim, Summary{
		Text: "summary", Items: []SummaryItem{{Ordinal: 0, Description: "description"}},
	}, time.Now())
	require.NoError(t, err)
	require.True(t, activated)
	originalItems, err := env.queries.ListContextProjectionItemsByNode(t.Context(), originalClaim.nodeID)
	require.NoError(t, err)
	require.Len(t, originalItems, 1)

	forked, err := env.sessions.Fork(t.Context(), env.sessionID)
	require.NoError(t, err)
	for _, count := range []func(context.Context, string) (int64, error){
		env.queries.CountContextProjectionNodesBySession,
		env.queries.CountContextProjectionSourcesBySession,
		env.queries.CountContextProjectionItemsBySession,
	} {
		originalCount, err := count(t.Context(), env.sessionID)
		require.NoError(t, err)
		require.Positive(t, originalCount)
		forkCount, err := count(t.Context(), forked.ID)
		require.NoError(t, err)
		require.Zero(t, forkCount)
	}
	forkUsage, err := env.queries.CountContextProjectionUsageAttemptsBySession(t.Context(), forked.ID)
	require.NoError(t, err)
	require.Zero(t, forkUsage)

	forkMessages, err := env.messages.List(t.Context(), forked.ID)
	require.NoError(t, err)
	forkCandidates := scanCandidates(forkMessages)
	require.Len(t, forkCandidates, 1)
	forkClaim, ok, err := env.store.claim(t.Context(), forkCandidates[0], time.Now(), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	activated, err = env.store.activate(t.Context(), forkClaim, Summary{
		Text: "summary", Items: []SummaryItem{{Ordinal: 0, Description: "description"}},
	}, time.Now())
	require.NoError(t, err)
	require.True(t, activated)
	forkSources, err := env.queries.ListContextProjectionSourcesByNode(t.Context(), forkClaim.nodeID)
	require.NoError(t, err)
	require.NotEmpty(t, forkSources)
	forkMessageIDs := make(map[string]struct{}, len(forkMessages))
	for _, msg := range forkMessages {
		forkMessageIDs[msg.ID] = struct{}{}
	}
	for _, source := range forkSources {
		_, belongsToFork := forkMessageIDs[source.SourceMessageID]
		require.True(t, belongsToFork)
	}
	forkItems, err := env.queries.ListContextProjectionItemsByNode(t.Context(), forkClaim.nodeID)
	require.NoError(t, err)
	require.Len(t, forkItems, 1)
	require.Greater(t, forkItems[0].RefNumber, originalItems[0].RefNumber)
	_, err = env.queries.GetActiveContextProjectionItemByRef(t.Context(), db.GetActiveContextProjectionItemByRefParams{
		SessionID: forked.ID, RefNumber: originalItems[0].RefNumber,
	})
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestSessionDeleteInvalidatesProjectionBeforeDeletingMessages(t *testing.T) {
	t.Parallel()

	env := newTestEnvironment(t, nil, enabledOptions())
	batch := env.createBatch(t, strings.Repeat("canonical", 100))
	candidate := scanCandidates(batch)[0]
	claimed, ok, err := env.store.claim(t.Context(), candidate, time.Now(), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	activated, err := env.store.activate(t.Context(), claimed, Summary{
		Text: "summary", Items: []SummaryItem{{Ordinal: 0, Description: "description"}},
	}, time.Now())
	require.NoError(t, err)
	require.True(t, activated)

	sessions := session.NewService(env.queries, env.store.db, session.WithDeleteLifecycle(env.store))
	require.NoError(t, sessions.Delete(t.Context(), env.sessionID))

	_, err = sessions.Get(t.Context(), env.sessionID)
	require.ErrorIs(t, err, sql.ErrNoRows)
	for _, count := range []func(context.Context, string) (int64, error){
		env.queries.CountContextProjectionNodesBySession,
		env.queries.CountContextProjectionSourcesBySession,
		env.queries.CountContextProjectionItemsBySession,
		env.queries.CountContextProjectionUsageAttemptsBySession,
	} {
		value, err := count(t.Context(), env.sessionID)
		require.NoError(t, err)
		require.Zero(t, value)
	}
}

func TestStoreNonSavingProjectionDoesNotAdvanceCounter(t *testing.T) {
	t.Parallel()

	env := newTestEnvironment(t, nil, enabledOptions())
	candidate := scanCandidates(env.createBatch(t, "x"))[0]
	before, err := env.queries.GetContextProjectionRefCounter(t.Context())
	require.NoError(t, err)
	claimed, ok, err := env.store.claim(t.Context(), candidate, time.Now(), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	activated, err := env.store.activate(t.Context(), claimed, Summary{
		Text: "summary", Items: []SummaryItem{{Ordinal: 0, Description: "description"}},
	}, time.Now())
	require.NoError(t, err)
	require.False(t, activated)
	after, err := env.queries.GetContextProjectionRefCounter(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
	node, err := env.store.node(t.Context(), candidate)
	require.NoError(t, err)
	require.Equal(t, "skipped", node.State)
	_, err = env.queries.GetActiveContextProjectionItemByRef(t.Context(), db.GetActiveContextProjectionItemByRefParams{SessionID: env.sessionID, RefNumber: before})
	require.True(t, errors.Is(err, sql.ErrNoRows))
}

func TestStoreAccountsProjectionUsageExactlyOnce(t *testing.T) {
	t.Parallel()

	env := newTestEnvironment(t, nil, enabledOptions())
	candidate := scanCandidates(env.createBatch(t, strings.Repeat("canonical", 300)))[0]
	claimed, ok, err := env.store.claim(t.Context(), candidate, time.Now(), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	activated, err := env.store.activate(t.Context(), claimed, Summary{
		Text: "summary", Items: []SummaryItem{{Ordinal: 0, Description: "description"}},
		Provider: "provider", Model: "model", PromptTokens: 3, CompletionTokens: 2, Cost: 0.25,
	}, time.Now())
	require.NoError(t, err)
	require.True(t, activated)

	for range 2 {
		require.NoError(t, env.store.AccountUsage(t.Context(), env.sessionID))
	}
	sess, err := env.sessions.Get(t.Context(), env.sessionID)
	require.NoError(t, err)
	require.Equal(t, int64(3), sess.PromptTokens)
	require.Equal(t, int64(2), sess.CompletionTokens)
	require.Equal(t, 0.25, sess.Cost)
	attempts, err := env.queries.ListUnaccountedContextProjectionUsage(t.Context(), env.sessionID)
	require.NoError(t, err)
	require.Empty(t, attempts)
}

func TestStorePersistsAndAccountsFailedAttemptUsage(t *testing.T) {
	t.Parallel()

	now := time.Unix(5_000, 0)
	summarizer := &sequenceSummarizer{results: []summaryResult{
		{err: RetryableSummaryErrorWithUsage(errors.New("malformed"), Usage{
			Provider: "provider", Model: "model", PromptTokens: 5, CompletionTokens: 4, Cost: 0.5,
		})},
		{err: RetryableSummaryErrorWithUsage(errors.New("malformed again"), Usage{
			Provider: "provider", Model: "model", PromptTokens: 7, CompletionTokens: 6, Cost: 0.75,
		})},
	}}
	env := newTestEnvironment(t, summarizer, enabledOptions())
	env.module.now = func() time.Time { return now }
	canonical := env.createBatch(t, strings.Repeat("canonical", 300))
	candidate := scanCandidates(canonical)[0]

	_, err := env.module.Project(t.Context(), env.sessionID, canonical)
	require.NoError(t, err)
	node, err := env.store.node(t.Context(), candidate)
	require.NoError(t, err)
	require.Equal(t, "failed", node.State)
	require.Equal(t, int64(5), node.PromptTokens)
	require.Equal(t, int64(4), node.CompletionTokens)
	require.Equal(t, 0.5, node.Cost)

	now = now.Add(5 * time.Minute)
	_, err = env.module.Project(t.Context(), env.sessionID, canonical)
	require.NoError(t, err)
	require.NoError(t, env.module.AccountUsage(t.Context(), env.sessionID))
	require.NoError(t, env.module.AccountUsage(t.Context(), env.sessionID))
	sess, err := env.sessions.Get(t.Context(), env.sessionID)
	require.NoError(t, err)
	require.Equal(t, int64(12), sess.PromptTokens)
	require.Equal(t, int64(10), sess.CompletionTokens)
	require.Equal(t, 1.25, sess.Cost)
}

func TestStoreRetriesUsageAccountingAfterRollback(t *testing.T) {
	t.Parallel()

	env := newTestEnvironment(t, nil, enabledOptions())
	candidate := scanCandidates(env.createBatch(t, strings.Repeat("canonical", 300)))[0]
	claimed, ok, err := env.store.claim(t.Context(), candidate, time.Now(), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	activated, err := env.store.activate(t.Context(), claimed, Summary{
		Text: "summary", Items: []SummaryItem{{Ordinal: 0, Description: "description"}},
		Provider: "provider", Model: "model", PromptTokens: 3, CompletionTokens: 2, Cost: 0.25,
	}, time.Now())
	require.NoError(t, err)
	require.True(t, activated)

	_, err = env.store.db.ExecContext(t.Context(), `
		CREATE TRIGGER fail_projection_usage_accounting
		BEFORE UPDATE OF accounted ON context_projection_usage_attempts
		BEGIN
			SELECT RAISE(ABORT, 'forced accounting failure');
		END`)
	require.NoError(t, err)
	require.ErrorContains(t, env.store.AccountUsage(t.Context(), env.sessionID), "forced accounting failure")
	sess, err := env.sessions.Get(t.Context(), env.sessionID)
	require.NoError(t, err)
	require.Zero(t, sess.PromptTokens)
	require.Zero(t, sess.CompletionTokens)
	require.Zero(t, sess.Cost)
	attempts, err := env.queries.ListUnaccountedContextProjectionUsage(t.Context(), env.sessionID)
	require.NoError(t, err)
	require.Len(t, attempts, 1)

	_, err = env.store.db.ExecContext(t.Context(), `DROP TRIGGER fail_projection_usage_accounting`)
	require.NoError(t, err)
	require.NoError(t, env.store.AccountUsage(t.Context(), env.sessionID))
	sess, err = env.sessions.Get(t.Context(), env.sessionID)
	require.NoError(t, err)
	require.Equal(t, int64(3), sess.PromptTokens)
	require.Equal(t, int64(2), sess.CompletionTokens)
	require.Equal(t, 0.25, sess.Cost)
}

type summaryResult struct {
	summary Summary
	err     error
}

type sequenceSummarizer struct {
	results []summaryResult
	calls   int
}

func (s *sequenceSummarizer) Summarize(context.Context, Candidate) (Summary, error) {
	result := s.results[s.calls]
	s.calls++
	return result.summary, result.err
}
