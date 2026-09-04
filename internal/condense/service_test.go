package condense

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProjectSummarizerFailureRecordsRetryAndReturnsRaw(t *testing.T) {
	t.Parallel()

	summarizer := &fixedSummarizer{err: errors.New("provider unavailable")}
	env := newTestEnvironment(t, summarizer, enabledOptions())
	now := time.Unix(10_000, 0)
	env.module.now = func() time.Time { return now }
	canonical := env.createBatch(t, strings.Repeat("canonical", 300))
	candidate := scanCandidates(canonical)[0]

	projected, err := env.module.Project(t.Context(), env.sessionID, canonical)
	require.NoError(t, err)
	require.Same(t, &canonical[0], &projected[0])
	node, err := env.store.node(t.Context(), candidate)
	require.NoError(t, err)
	require.Equal(t, "failed", node.State)
	require.Equal(t, now.Add(5*time.Minute).Unix(), node.RetryAfter.Int64)
	require.NotContains(t, node.Failure, canonical[1].ToolResults()[0].Content)

	_, err = env.module.Project(t.Context(), env.sessionID, canonical)
	require.NoError(t, err)
	require.Equal(t, 1, summarizer.calls, "retry delay suppresses generation")

	now = now.Add(5 * time.Minute)
	_, err = env.module.Project(t.Context(), env.sessionID, canonical)
	require.NoError(t, err)
	require.Equal(t, 2, summarizer.calls)
	node, err = env.store.node(t.Context(), candidate)
	require.NoError(t, err)
	require.Equal(t, int64(2), node.AttemptCount)
	require.Equal(t, now.Add(10*time.Minute).Unix(), node.RetryAfter.Int64)
}

func TestProjectBelowThresholdSkipsOneCandidate(t *testing.T) {
	t.Parallel()

	summarizer := &fixedSummarizer{summary: Summary{Text: "summary"}}
	options := enabledOptions()
	options.MinBatchChars = 10_000
	env := newTestEnvironment(t, summarizer, options)
	first := env.createBatch(t, "first")
	second := env.createBatch(t, "second")
	messages := append(first, second...)

	projected, err := env.module.Project(t.Context(), env.sessionID, messages)
	require.NoError(t, err)
	require.Same(t, &messages[0], &projected[0])
	require.Zero(t, summarizer.calls)
	firstNode, err := env.store.node(t.Context(), scanCandidates(first)[0])
	require.NoError(t, err)
	require.Equal(t, "skipped", firstNode.State)
	_, err = env.store.node(t.Context(), scanCandidates(second)[0])
	require.Error(t, err, "at most one new candidate is recorded per turn")
}

func TestProjectPersistsTruncatedSummaryUsage(t *testing.T) {
	t.Parallel()

	summarizer := &fixedSummarizer{summary: Summary{
		Text: "truncated", Truncated: true, Provider: "provider", Model: "model",
		PromptTokens: 9, CompletionTokens: 8, Cost: 0.75,
	}}
	env := newTestEnvironment(t, summarizer, enabledOptions())
	canonical := env.createBatch(t, strings.Repeat("canonical", 300))

	projected, err := env.module.Project(t.Context(), env.sessionID, canonical)
	require.NoError(t, err)
	require.Equal(t, canonical, projected)
	require.NoError(t, env.module.AccountUsage(t.Context(), env.sessionID))
	sess, err := env.sessions.Get(t.Context(), env.sessionID)
	require.NoError(t, err)
	require.Equal(t, int64(9), sess.PromptTokens)
	require.Equal(t, int64(8), sess.CompletionTokens)
	require.Equal(t, 0.75, sess.Cost)
}

func TestProjectPersistsUsageBeforeReturningParentCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancelCause(t.Context())
	summarizer := &cancelAfterResponseSummarizer{cancel: cancel}
	env := newTestEnvironment(t, summarizer, enabledOptions())
	canonical := env.createBatch(t, strings.Repeat("canonical", 300))

	projected, err := env.module.Project(ctx, env.sessionID, canonical)
	require.Equal(t, canonical, projected)
	require.ErrorContains(t, err, "parent canceled")
	require.True(t, IsFatal(err))
	require.NoError(t, env.module.AccountUsage(t.Context(), env.sessionID))
	sess, accountErr := env.sessions.Get(t.Context(), env.sessionID)
	require.NoError(t, accountErr)
	require.Equal(t, int64(11), sess.PromptTokens)
	require.Equal(t, int64(10), sess.CompletionTokens)
	require.Equal(t, 1.25, sess.Cost)
}

type cancelAfterResponseSummarizer struct {
	cancel context.CancelCauseFunc
}

func (s *cancelAfterResponseSummarizer) Summarize(_ context.Context, candidate Candidate) (Summary, error) {
	s.cancel(errors.New("parent canceled"))
	items := make([]SummaryItem, len(candidate.Items))
	for index := range items {
		items[index] = SummaryItem{Ordinal: index, Description: "description"}
	}
	return Summary{
		Text: "summary", Items: items, Provider: "provider", Model: "model",
		PromptTokens: 11, CompletionTokens: 10, Cost: 1.25,
	}, nil
}

func TestProjectLiveOldestClaimDoesNotProcessLaterCandidate(t *testing.T) {
	t.Parallel()

	summarizer := &fixedSummarizer{summary: Summary{Text: "summary"}}
	env := newTestEnvironment(t, summarizer, enabledOptions())
	first := env.createBatch(t, strings.Repeat("first", 300))
	second := env.createBatch(t, strings.Repeat("second", 300))
	messages := append(first, second...)

	_, ok, err := env.store.claimForHistory(
		t.Context(), scanCandidates(first)[0], env.module.Options().ConfigurationID,
		messages, time.Now(), time.Minute,
	)
	require.NoError(t, err)
	require.True(t, ok)

	projected, err := env.module.Project(t.Context(), env.sessionID, messages)
	require.NoError(t, err)
	require.Equal(t, messages, projected)
	require.Zero(t, summarizer.calls)
	_, err = env.store.node(t.Context(), scanCandidates(second)[0])
	require.Error(t, err)
}
