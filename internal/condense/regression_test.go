package condense

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestProjectionValidationRequiresCompleteSourcesHashesAndBoundaries(t *testing.T) {
	t.Parallel()

	messages := regressionSplitBatch()
	candidate := scanCandidates(messages)[0]
	projection := regressionActiveProjection(candidate, "node", 1)
	require.True(t, projectionValid(messages, projection))

	tests := []struct {
		name   string
		mutate func([]message.Message, activeProjection) ([]message.Message, activeProjection)
	}{
		{
			name: "missing source row",
			mutate: func(messages []message.Message, projection activeProjection) ([]message.Message, activeProjection) {
				projection.sources = projection.sources[:len(projection.sources)-1]
				return messages, projection
			},
		},
		{
			name: "source hash mismatch",
			mutate: func(messages []message.Message, projection activeProjection) ([]message.Message, activeProjection) {
				projection.sources[0].SourceHash = "wrong"
				return messages, projection
			},
		},
		{
			name: "source ownership mismatch",
			mutate: func(messages []message.Message, projection activeProjection) ([]message.Message, activeProjection) {
				projection.sources[0].NodeID = "another-node"
				return messages, projection
			},
		},
		{
			name: "node aggregate hash mismatch",
			mutate: func(messages []message.Message, projection activeProjection) ([]message.Message, activeProjection) {
				projection.node.SourceHash = "wrong"
				return messages, projection
			},
		},
		{
			name: "first message boundary mismatch",
			mutate: func(messages []message.Message, projection activeProjection) ([]message.Message, activeProjection) {
				projection.node.FirstMessageID = messages[1].ID
				return messages, projection
			},
		},
		{
			name: "last message boundary mismatch",
			mutate: func(messages []message.Message, projection activeProjection) ([]message.Message, activeProjection) {
				projection.node.LastMessageID = messages[1].ID
				return messages, projection
			},
		},
		{
			name: "canonical assistant part changed",
			mutate: func(messages []message.Message, projection activeProjection) ([]message.Message, activeProjection) {
				messages = regressionCloneMessages(messages)
				call := messages[0].Parts[0].(message.ToolCall)
				call.Input = `{"path":"changed"}`
				messages[0].Parts[0] = call
				return messages, projection
			},
		},
		{
			name: "canonical result metadata changed",
			mutate: func(messages []message.Message, projection activeProjection) ([]message.Message, activeProjection) {
				messages = regressionCloneMessages(messages)
				result := messages[1].Parts[0].(message.ToolResult)
				result.Metadata = `{"changed":true}`
				messages[1].Parts[0] = result
				return messages, projection
			},
		},
		{
			name: "item hash mismatch",
			mutate: func(messages []message.Message, projection activeProjection) ([]message.Message, activeProjection) {
				projection.items[0].SourceHash = "wrong"
				return messages, projection
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutatedMessages := regressionCloneMessages(messages)
			mutatedProjection := regressionCloneProjection(projection)
			mutatedMessages, mutatedProjection = test.mutate(mutatedMessages, mutatedProjection)
			require.False(t, projectionValid(mutatedMessages, mutatedProjection))

			projected := applyProjections(mutatedMessages, []activeProjection{mutatedProjection})
			for index := range mutatedMessages {
				require.Same(t, &mutatedMessages[index], &projected[index], "an invalid node must be applied all-or-none")
			}
			for _, msg := range projected {
				for _, result := range msg.ToolResults() {
					require.NotContains(t, result.Content, "[Context projection]")
					require.NotContains(t, result.Content, "preceding context summary")
				}
			}
		})
	}
}

func TestProjectionSummaryBoundaryIsAllOrNone(t *testing.T) {
	t.Parallel()

	messages := regressionSplitBatch()
	candidate := scanCandidates(messages)[0]
	projection := regressionActiveProjection(candidate, "node", 1)

	full := applyProjections(messages, []activeProjection{projection})
	require.Contains(t, full[1].ToolResults()[0].Content, "[Context projection]")
	require.Contains(t, full[2].ToolResults()[0].Content, "preceding context summary")

	unrelated := message.Message{ID: "unrelated", SessionID: "session", Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "between"}}}
	withGap := []message.Message{messages[0], messages[1], unrelated, messages[2]}
	gapped := applyProjections(withGap, []activeProjection{projection})
	require.Same(t, &withGap[0], &gapped[0], "stored source messages must remain one complete canonical batch")
	require.Same(t, &withGap[1], &gapped[1])
	require.Same(t, &withGap[2], &gapped[2])
	require.Same(t, &withGap[3], &gapped[3])

	for name, boundary := range map[string][]message.Message{
		"missing summary-bearing first result": {messages[0], messages[2]},
		"missing trailing result":              messages[:2],
	} {
		t.Run(name, func(t *testing.T) {
			projected := applyProjections(boundary, []activeProjection{projection})
			for index := range boundary {
				require.Same(t, &boundary[index], &projected[index])
			}
			for _, msg := range projected {
				for _, result := range msg.ToolResults() {
					require.NotContains(t, result.Content, "[Context projection]")
					require.NotContains(t, result.Content, "preceding context summary")
				}
			}
		})
	}
}

func TestProjectPartialActiveBoundaryStaysRawWithoutStaling(t *testing.T) {
	t.Parallel()

	summarizer := &fixedSummarizer{summary: Summary{Text: "complete summary"}}
	env := newTestEnvironment(t, summarizer, enabledOptions())
	assistant, err := env.messages.Create(t.Context(), env.sessionID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: "call-a", Name: "view", Input: `{"path":"a"}`, Finished: true},
			message.ToolCall{ID: "call-b", Name: "grep", Input: `{"query":"b"}`, Finished: true},
		},
	})
	require.NoError(t, err)
	assistant.AddFinish(message.FinishReasonToolUse, "", "")
	require.NoError(t, env.messages.Update(t.Context(), assistant))
	first, err := env.messages.Create(t.Context(), env.sessionID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{message.ToolResult{
			ToolCallID: "call-a", Name: "view", Content: strings.Repeat("first canonical", 200),
		}},
	})
	require.NoError(t, err)
	second, err := env.messages.Create(t.Context(), env.sessionID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{message.ToolResult{
			ToolCallID: "call-b", Name: "grep", Content: strings.Repeat("second canonical", 200),
		}},
	})
	require.NoError(t, err)
	canonical := []message.Message{assistant, first, second}
	candidate := scanCandidates(canonical)[0]

	full, err := env.module.Project(t.Context(), env.sessionID, canonical)
	require.NoError(t, err)
	require.Contains(t, full[1].ToolResults()[0].Content, "[Context projection]")
	require.Contains(t, full[2].ToolResults()[0].Content, "preceding context summary")

	for name, boundary := range map[string][]message.Message{
		"missing summary-bearing first result": {assistant, second},
		"missing trailing result":              {assistant, first},
	} {
		t.Run(name, func(t *testing.T) {
			projected, projectErr := env.module.Project(t.Context(), env.sessionID, boundary)
			require.NoError(t, projectErr)
			require.Equal(t, boundary, projected)
			for _, msg := range projected {
				for _, result := range msg.ToolResults() {
					require.NotContains(t, result.Content, "[Context projection]")
					require.NotContains(t, result.Content, "preceding context summary")
				}
			}
			node, nodeErr := env.store.node(t.Context(), candidate)
			require.NoError(t, nodeErr)
			require.Equal(t, "active", node.State, "a partial view is not proof that canonical content changed")
		})
	}

	full, err = env.module.Project(t.Context(), env.sessionID, canonical)
	require.NoError(t, err)
	require.Contains(t, full[1].ToolResults()[0].Content, "[Context projection]")
	require.Contains(t, full[2].ToolResults()[0].Content, "preceding context summary")
	require.Equal(t, 1, summarizer.calls)
}

func TestProjectCleansInvalidActiveNodeAndAllocatesHigherReplacementRefs(t *testing.T) {
	t.Parallel()

	summarizer := &fixedSummarizer{summary: Summary{Text: "summary"}}
	env := newTestEnvironment(t, summarizer, enabledOptions())
	canonical := env.createBatch(t, strings.Repeat("canonical", 400))
	oldCandidate := scanCandidates(canonical)[0]

	_, err := env.module.Project(t.Context(), env.sessionID, canonical)
	require.NoError(t, err)
	oldNode, err := env.store.node(t.Context(), oldCandidate)
	require.NoError(t, err)
	oldItems, err := env.queries.ListContextProjectionItemsByNode(t.Context(), oldNode.ID)
	require.NoError(t, err)
	require.Len(t, oldItems, 1)
	oldRef := oldItems[0].RefNumber
	oldSources, err := env.queries.ListContextProjectionSourcesByNode(t.Context(), oldNode.ID)
	require.NoError(t, err)
	require.NotEmpty(t, oldSources)

	changed := regressionCloneMessages(canonical)
	call := changed[0].Parts[0].(message.ToolCall)
	call.Input = `{"path":"replacement"}`
	changed[0].Parts[0] = call
	require.NoError(t, env.messages.Update(t.Context(), changed[0]))
	newCandidate := scanCandidates(changed)[0]
	require.NotEqual(t, oldCandidate.SourceHash, newCandidate.SourceHash)

	projected, err := env.module.Project(t.Context(), env.sessionID, changed)
	require.NoError(t, err)
	require.Contains(t, projected[1].ToolResults()[0].Content, "[Context projection]")
	require.Equal(t, 2, summarizer.calls)

	oldNode, err = env.store.node(t.Context(), oldCandidate)
	require.NoError(t, err)
	require.Equal(t, "stale", oldNode.State)
	oldItems, err = env.queries.ListContextProjectionItemsByNode(t.Context(), oldNode.ID)
	require.NoError(t, err)
	require.Empty(t, oldItems)
	oldSources, err = env.queries.ListContextProjectionSourcesByNode(t.Context(), oldNode.ID)
	require.NoError(t, err)
	require.Empty(t, oldSources)

	newNode, err := env.store.node(t.Context(), newCandidate)
	require.NoError(t, err)
	require.Equal(t, "active", newNode.State)
	require.NotEqual(t, oldNode.ID, newNode.ID)
	newItems, err := env.queries.ListContextProjectionItemsByNode(t.Context(), newNode.ID)
	require.NoError(t, err)
	require.Len(t, newItems, 1)
	require.Greater(t, newItems[0].RefNumber, oldRef)

	oldResult, err := env.module.Query(t.Context(), env.sessionID, Query{Ref: refString(oldRef)})
	require.NoError(t, err)
	require.Equal(t, "not_found", oldResult.Status)
	newResult, err := env.module.Query(t.Context(), env.sessionID, Query{Ref: refString(newItems[0].RefNumber)})
	require.NoError(t, err)
	require.Equal(t, "ok", newResult.Status)
	require.Equal(t, changed[1].ToolResults()[0].Content, newResult.Content)
}

func TestProjectTreatsMalformedPersistedCodecDuringActivationAsRecoverable(t *testing.T) {
	t.Parallel()

	env := newTestEnvironment(t, nil, enabledOptions())
	canonical := env.createBatch(t, strings.Repeat("verified canonical content", 200))
	candidate := scanCandidates(canonical)[0]
	summarizer := &malformedCodecSummarizer{env: env, messageID: canonical[1].ID}
	env.module.summarizer = summarizer

	projected, err := env.module.Project(t.Context(), env.sessionID, canonical)
	require.Error(t, err)
	require.True(t, IsRecoverable(err))
	require.False(t, IsFatal(err))
	var projectionErr *ProjectionError
	require.ErrorAs(t, err, &projectionErr)
	require.Equal(t, ErrorClassStore, projectionErr.Class)
	require.Contains(t, err.Error(), "reload activation source")
	require.Contains(t, err.Error(), "decode source message")
	require.NotContains(t, err.Error(), canonical[1].ToolResults()[0].Content)
	require.Equal(t, canonical, projected)

	node, nodeErr := env.store.node(t.Context(), candidate)
	require.NoError(t, nodeErr)
	require.Equal(t, "pending", node.State, "a codec error must not stale previously verified content")
	items, queryErr := env.queries.ListContextProjectionItemsByNode(t.Context(), node.ID)
	require.NoError(t, queryErr)
	require.Empty(t, items)
	sources, queryErr := env.queries.ListContextProjectionSourcesByNode(t.Context(), node.ID)
	require.NoError(t, queryErr)
	require.Empty(t, sources)
	counter, queryErr := env.queries.GetContextProjectionRefCounter(t.Context())
	require.NoError(t, queryErr)
	require.Equal(t, int64(1), counter)
}

type malformedCodecSummarizer struct {
	env       *testEnvironment
	messageID string
}

func (s *malformedCodecSummarizer) Summarize(ctx context.Context, candidate Candidate) (Summary, error) {
	_, err := s.env.store.db.ExecContext(ctx, `UPDATE messages SET parts = ? WHERE id = ?`, "{malformed", s.messageID)
	if err != nil {
		return Summary{}, err
	}
	return regressionSummary(candidate, "output that must not activate"), nil
}

func TestProjectParentCancellationWhileWaitingForDatabaseIsFatal(t *testing.T) {
	env := newTestEnvironment(t, nil, enabledOptions())
	canonical := []message.Message{{ID: "user", SessionID: env.sessionID, Role: message.User}}

	conn, err := env.store.db.Conn(t.Context())
	require.NoError(t, err)
	defer conn.Close()
	baseline := env.store.db.Stats().WaitCount

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, projectErr := env.module.Project(ctx, env.sessionID, canonical)
		result <- projectErr
	}()

	require.Eventually(t, func() bool {
		return env.store.db.Stats().WaitCount > baseline
	}, time.Second, time.Millisecond, "Project should be blocked in the DB path")
	cancel()

	select {
	case err = <-result:
		require.Error(t, err)
		require.True(t, IsFatal(err))
		require.False(t, IsRecoverable(err))
		require.ErrorIs(t, err, context.Canceled)
		var projectionErr *ProjectionError
		require.ErrorAs(t, err, &projectionErr)
		require.Equal(t, ErrorClassStore, projectionErr.Class)
	case <-time.After(time.Second):
		t.Fatal("Project did not return after parent cancellation")
	}
}

func TestProjectRejectsSummaryReturnedAfterChildTimeout(t *testing.T) {
	t.Parallel()

	options := enabledOptions()
	options.SummarizerTimeout = 20 * time.Millisecond
	summarizer := &timeoutOutputSummarizer{}
	env := newTestEnvironment(t, summarizer, options)
	now := time.Unix(20_000, 0)
	env.module.now = func() time.Time { return now }
	canonical := env.createBatch(t, strings.Repeat("canonical", 300))
	candidate := scanCandidates(canonical)[0]

	projected, err := env.module.Project(t.Context(), env.sessionID, canonical)
	require.NoError(t, err)
	require.Equal(t, canonical, projected)
	require.Equal(t, 1, summarizer.calls)

	node, err := env.store.node(t.Context(), candidate)
	require.NoError(t, err)
	require.Equal(t, "failed", node.State)
	require.Equal(t, "summarizer retryable failure", node.Failure)
	require.True(t, node.RetryAfter.Valid)
	require.Equal(t, now.Add(5*time.Minute).Unix(), node.RetryAfter.Int64)
	require.Empty(t, node.Summary)
	require.Equal(t, "test-provider", node.SummarizerProvider)
	require.Equal(t, "test-model", node.SummarizerModel)
	items, err := env.queries.ListContextProjectionItemsByNode(t.Context(), node.ID)
	require.NoError(t, err)
	require.Empty(t, items)
}

func TestProjectParentCancellationDuringSummaryIsFatal(t *testing.T) {
	t.Parallel()

	summarizer := &startedBlockingSummarizer{started: make(chan struct{})}
	env := newTestEnvironment(t, summarizer, enabledOptions())
	canonical := env.createBatch(t, strings.Repeat("canonical", 300))
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, err := env.module.Project(ctx, env.sessionID, canonical)
		result <- err
	}()
	select {
	case <-summarizer.started:
	case <-time.After(time.Second):
		t.Fatal("summarizer did not start")
	}
	cancel()
	select {
	case err := <-result:
		require.Error(t, err)
		require.True(t, IsFatal(err))
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("Project did not propagate parent cancellation")
	}
}

type startedBlockingSummarizer struct {
	started chan struct{}
}

func (s *startedBlockingSummarizer) Summarize(ctx context.Context, _ Candidate) (Summary, error) {
	close(s.started)
	<-ctx.Done()
	return Summary{}, ctx.Err()
}

type timeoutOutputSummarizer struct {
	calls int
}

func (s *timeoutOutputSummarizer) Summarize(ctx context.Context, candidate Candidate) (Summary, error) {
	s.calls++
	<-ctx.Done()
	return regressionSummary(candidate, "late output must be rejected"), nil
}

func TestProjectSummaryErrorPolicyAndSanitizedPersistence(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		wrap      func(error) error
		failure   string
		retryable bool
	}{
		{name: "retryable", wrap: RetryableSummaryError, failure: "summarizer retryable failure", retryable: true},
		{name: "terminal", wrap: TerminalSummaryError, failure: "summarizer terminal failure", retryable: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newTestEnvironment(t, nil, enabledOptions())
			now := time.Unix(30_000, 0)
			env.module.now = func() time.Time { return now }
			canonical := env.createBatch(t, strings.Repeat("secret canonical result", 200))
			candidate := scanCandidates(canonical)[0]
			secret := canonical[1].ToolResults()[0].Content
			summarizer := &fixedSummarizer{err: test.wrap(errors.New("provider echoed canonical content: " + secret))}
			env.module.summarizer = summarizer

			projected, err := env.module.Project(t.Context(), env.sessionID, canonical)
			require.NoError(t, err)
			require.Equal(t, canonical, projected)
			require.Equal(t, 1, summarizer.calls)

			node, err := env.store.node(t.Context(), candidate)
			require.NoError(t, err)
			require.Equal(t, "failed", node.State)
			require.Equal(t, test.failure, node.Failure)
			require.NotContains(t, node.Failure, secret)
			require.Empty(t, node.Summary)
			require.Empty(t, node.SummarizerProvider)
			require.Empty(t, node.SummarizerModel)
			items, err := env.queries.ListContextProjectionItemsByNode(t.Context(), node.ID)
			require.NoError(t, err)
			require.Empty(t, items)

			if test.retryable {
				require.True(t, node.RetryAfter.Valid)
				require.Equal(t, now.Add(5*time.Minute).Unix(), node.RetryAfter.Int64)
				_, err = env.module.Project(t.Context(), env.sessionID, canonical)
				require.NoError(t, err)
				require.Equal(t, 1, summarizer.calls, "retryable failures honor their retry fence")
				now = now.Add(5 * time.Minute)
				_, err = env.module.Project(t.Context(), env.sessionID, canonical)
				require.NoError(t, err)
				require.Equal(t, 2, summarizer.calls)
			} else {
				require.False(t, node.RetryAfter.Valid)
				now = now.Add(24 * time.Hour)
				_, err = env.module.Project(t.Context(), env.sessionID, canonical)
				require.NoError(t, err)
				require.Equal(t, 1, summarizer.calls, "terminal failures remain suppressed")
			}
		})
	}
}

func TestProjectExposesFailureRecordingFenceWithoutProviderContent(t *testing.T) {
	t.Parallel()

	env := newTestEnvironment(t, nil, enabledOptions())
	canonical := env.createBatch(t, strings.Repeat("canonical private content", 200))
	candidate := scanCandidates(canonical)[0]
	secret := canonical[1].ToolResults()[0].Content
	summarizer := &claimStealingSummarizer{env: env, replacementToken: "replacement-token", providerError: errors.New("provider echoed: " + secret)}
	env.module.summarizer = summarizer

	projected, err := env.module.Project(t.Context(), env.sessionID, canonical)
	require.NoError(t, summarizer.mutationErr)
	require.Error(t, err)
	require.True(t, IsRecoverable(err))
	require.False(t, IsFatal(err))
	var projectionErr *ProjectionError
	require.ErrorAs(t, err, &projectionErr)
	require.Equal(t, ErrorClassStore, projectionErr.Class)
	require.Contains(t, err.Error(), "record sanitized summary failure")
	require.Contains(t, err.Error(), "claim fence rejected transition")
	require.NotContains(t, err.Error(), secret)
	require.Equal(t, canonical, projected)

	node, nodeErr := env.store.node(t.Context(), candidate)
	require.NoError(t, nodeErr)
	require.Equal(t, "pending", node.State)
	require.True(t, node.ClaimToken.Valid)
	require.Equal(t, summarizer.replacementToken, node.ClaimToken.String)
	require.Empty(t, node.Failure)
}

type claimStealingSummarizer struct {
	env              *testEnvironment
	replacementToken string
	providerError    error
	mutationErr      error
}

func (s *claimStealingSummarizer) Summarize(ctx context.Context, candidate Candidate) (Summary, error) {
	result, err := s.env.store.db.ExecContext(ctx, `
		UPDATE context_projection_nodes
		SET claim_token = ?
		WHERE session_id = ? AND batch_key = ? AND source_hash = ?
		  AND algorithm_version = ? AND state = 'pending'`,
		s.replacementToken, candidate.SessionID, candidate.BatchKey, candidate.SourceHash, AlgorithmVersion,
	)
	if err != nil {
		s.mutationErr = err
		return Summary{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		s.mutationErr = err
		return Summary{}, err
	}
	if rows != 1 {
		s.mutationErr = fmt.Errorf("updated %d claims", rows)
		return Summary{}, s.mutationErr
	}
	return Summary{}, RetryableSummaryError(s.providerError)
}

func TestQueryReportsUnavailableNilRecoveryStores(t *testing.T) {
	t.Parallel()

	for name, store := range map[string]*Store{
		"nil store":                   nil,
		"zero store":                  {},
		"store with nil dependencies": NewStore(nil, nil),
	} {
		t.Run(name, func(t *testing.T) {
			module := New(store, nil, Options{})
			result, err := module.Query(t.Context(), "session", Query{Ref: "t1"})
			require.Error(t, err)
			require.Contains(t, err.Error(), "store is unavailable")
			require.Equal(t, Result{}, result)
		})
	}
}

func TestStoreRefCounterCanAllocateFinalSignedReference(t *testing.T) {
	t.Parallel()

	env := newTestEnvironment(t, nil, enabledOptions())
	_, err := env.store.db.ExecContext(t.Context(), `
		UPDATE context_projection_ref_counter SET next_ref_number = ? WHERE singleton = 1`, int64Max-1)
	require.NoError(t, err)
	ref, err := env.queries.ReserveContextProjectionRefs(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, int64Max-1, ref)
	counter, err := env.queries.GetContextProjectionRefCounter(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64Max, counter)
}

func TestStoreActivationRejectsSignedRefOverflowAtomically(t *testing.T) {
	t.Parallel()

	env := newTestEnvironment(t, nil, enabledOptions())
	candidate := scanCandidates(env.createBatch(t, strings.Repeat("canonical", 500)))[0]
	claimed, ok, err := env.store.claim(t.Context(), candidate, time.Unix(40_000, 0), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = env.store.db.ExecContext(t.Context(), `
		UPDATE context_projection_ref_counter SET next_ref_number = ? WHERE singleton = 1`, int64Max)
	require.NoError(t, err)

	activated, err := env.store.activate(t.Context(), claimed, regressionSummary(candidate, "summary"), time.Unix(40_001, 0))
	require.False(t, activated)
	require.ErrorContains(t, err, "ref counter overflow")

	counter, queryErr := env.queries.GetContextProjectionRefCounter(t.Context())
	require.NoError(t, queryErr)
	require.Equal(t, int64Max, counter)
	node, queryErr := env.store.node(t.Context(), candidate)
	require.NoError(t, queryErr)
	require.Equal(t, "pending", node.State)
	items, queryErr := env.queries.ListContextProjectionItemsByNode(t.Context(), node.ID)
	require.NoError(t, queryErr)
	require.Empty(t, items)
	sources, queryErr := env.queries.ListContextProjectionSourcesByNode(t.Context(), node.ID)
	require.NoError(t, queryErr)
	require.Empty(t, sources)
}

func TestReloadCandidateRejectsWrongSessionSourceRows(t *testing.T) {
	t.Parallel()

	env := newTestEnvironment(t, nil, enabledOptions())
	canonical := env.createBatch(t, strings.Repeat("canonical", 100))
	candidate := scanCandidates(canonical)[0]
	other, err := session.NewService(env.queries, env.store.db).Create(t.Context(), "other session")
	require.NoError(t, err)
	_, err = env.store.db.ExecContext(t.Context(), `UPDATE messages SET session_id = ? WHERE id = ?`, other.ID, candidate.ToolMessages[0].ID)
	require.NoError(t, err)

	reloaded, err := env.store.reloadCandidate(t.Context(), env.queries, candidate)
	require.Error(t, err)
	require.Contains(t, err.Error(), "belongs to another session")
	require.Contains(t, err.Error(), candidate.ToolMessages[0].ID)
	require.Equal(t, Candidate{}, reloaded)
}

func regressionSplitBatch() []message.Message {
	assistant := message.Message{
		ID: "assistant", SessionID: "session", Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: "call-a", Name: "view", Input: `{"path":"a"}`, Finished: true},
			message.ToolCall{ID: "call-b", Name: "grep", Input: `{"query":"b"}`, Finished: true},
			message.Finish{Reason: message.FinishReasonToolUse},
		},
	}
	first := message.Message{
		ID: "tool-a", SessionID: "session", Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{ToolCallID: "call-a", Name: "view", Content: strings.Repeat("first", 100), Metadata: `{"canonical":true}`},
			message.Finish{Reason: message.FinishReasonEndTurn},
		},
	}
	second := message.Message{
		ID: "tool-b", SessionID: "session", Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{ToolCallID: "call-b", Name: "grep", Content: strings.Repeat("second", 100), IsError: true},
			message.Finish{Reason: message.FinishReasonEndTurn},
		},
	}
	return []message.Message{assistant, first, second}
}

func regressionActiveProjection(candidate Candidate, nodeID string, firstRef int64) activeProjection {
	projection := activeProjection{node: db.ContextProjectionNode{
		ID: nodeID, SessionID: candidate.SessionID, BatchKey: candidate.BatchKey,
		SourceHash: candidate.SourceHash, AlgorithmVersion: AlgorithmVersion, State: "active",
		Summary: "complete summary", FirstMessageID: candidate.FirstMessageID, LastMessageID: candidate.LastMessageID,
	}}
	projection.sources = make([]db.ContextProjectionSource, len(candidate.Sources))
	for index, source := range candidate.Sources {
		projection.sources[index] = db.ContextProjectionSource{
			NodeID: nodeID, SessionID: candidate.SessionID, SourceMessageID: source.MessageID,
			SourcePartOrdinal: int64(source.PartOrdinal), PartKind: source.PartKind,
			SourceHash: source.SourceHash, Ordinal: int64(index),
		}
	}
	projection.items = make([]db.ContextProjectionItem, len(candidate.Items))
	for index, item := range candidate.Items {
		projection.items[index] = db.ContextProjectionItem{
			ID: fmt.Sprintf("item-%d", index), NodeID: nodeID, SessionID: candidate.SessionID,
			SourceMessageID: item.MessageID, SourcePartOrdinal: int64(item.PartOrdinal),
			ToolCallID: item.ToolCallID, ToolName: item.ToolName, Description: fmt.Sprintf("description %d", index),
			RefNumber: firstRef + int64(index), SourceHash: item.SourceHash,
			Ordinal: int64(index), AlgorithmVersion: AlgorithmVersion,
		}
	}
	return projection
}

func regressionCloneProjection(projection activeProjection) activeProjection {
	projection.sources = append([]db.ContextProjectionSource(nil), projection.sources...)
	projection.items = append([]db.ContextProjectionItem(nil), projection.items...)
	return projection
}

func regressionCloneMessages(messages []message.Message) []message.Message {
	cloned := make([]message.Message, len(messages))
	for index := range messages {
		cloned[index] = messages[index].Clone()
	}
	return cloned
}

func regressionSummary(candidate Candidate, text string) Summary {
	items := make([]SummaryItem, len(candidate.Items))
	for index := range items {
		items[index] = SummaryItem{Ordinal: index, Description: fmt.Sprintf("description %d", index)}
	}
	return Summary{Text: text, Items: items, Provider: "test-provider", Model: "test-model"}
}
