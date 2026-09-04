package condense

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestActivationRejectsCanonicalHistoryChangesAfterSummary(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		mutate func(*testEnvironment, []message.Message)
	}{
		{
			name: "message appended",
			mutate: func(env *testEnvironment, _ []message.Message) {
				_, err := env.messages.Create(t.Context(), env.sessionID, message.CreateMessageParams{
					Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "concurrent"}},
				})
				require.NoError(t, err)
			},
		},
		{
			name: "message inserted into canonical order",
			mutate: func(env *testEnvironment, canonical []message.Message) {
				_, err := env.store.db.ExecContext(t.Context(), `
					INSERT INTO messages (
						id, session_id, role, parts, is_summary_message, created_at, updated_at
					) VALUES (?, ?, 'user', '[]', 0, ?, ?)`,
					"intervening", env.sessionID, canonical[0].CreatedAt, canonical[0].CreatedAt,
				)
				require.NoError(t, err)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnvironment(t, nil, enabledOptions())
			canonical := env.createBatch(t, strings.Repeat("canonical", 300))
			candidate := scanCandidates(canonical)[0]
			options := env.module.Options()
			claimed, ok, err := env.store.claimForHistory(
				t.Context(), candidate, options.ConfigurationID, canonical,
				time.Unix(1_000, 0), time.Minute,
			)
			require.NoError(t, err)
			require.True(t, ok)

			test.mutate(env, canonical)
			activated, err := env.store.activate(t.Context(), claimed, regressionSummary(candidate, "summary"), time.Unix(1_001, 0))
			require.False(t, activated)
			require.Error(t, err)
			require.True(t, IsRecoverable(err))
			node, nodeErr := env.store.nodeForConfiguration(t.Context(), candidate, options.ConfigurationID)
			require.NoError(t, nodeErr)
			require.Equal(t, "stale", node.State)
		})
	}
}

func TestActivationRejectsNonSourceHistoryContentChanges(t *testing.T) {
	t.Parallel()

	env := newTestEnvironment(t, nil, enabledOptions())
	_, err := env.messages.Create(t.Context(), env.sessionID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "before"}},
	})
	require.NoError(t, err)
	canonical, err := env.messages.List(t.Context(), env.sessionID)
	require.NoError(t, err)
	canonical = append(canonical, env.createBatch(t, strings.Repeat("canonical", 300))...)
	candidate := scanCandidates(canonical)[0]
	options := env.module.Options()
	claimed, ok, err := env.store.claimForHistory(
		t.Context(), candidate, options.ConfigurationID, canonical,
		time.Unix(1_500, 0), time.Minute,
	)
	require.NoError(t, err)
	require.True(t, ok)

	first := canonical[0]
	first.Parts = []message.ContentPart{
		message.TextContent{Text: "changed without changing the message ID"},
		message.Finish{Reason: message.FinishReasonEndTurn},
	}
	require.NoError(t, env.messages.Update(t.Context(), first))
	require.NoError(t, env.messages.Flush(t.Context(), first.ID))

	activated, err := env.store.activate(t.Context(), claimed, regressionSummary(candidate, "summary"), time.Unix(1_501, 0))
	require.False(t, activated)
	require.Error(t, err)
	require.True(t, IsRecoverable(err))
}

func TestActivationRejectsSummaryBoundaryMovement(t *testing.T) {
	t.Parallel()

	env := newTestEnvironment(t, nil, enabledOptions())
	canonical := env.createBatch(t, strings.Repeat("canonical", 300))
	candidate := scanCandidates(canonical)[0]
	options := env.module.Options()
	claimed, ok, err := env.store.claimForHistory(
		t.Context(), candidate, options.ConfigurationID, canonical,
		time.Unix(2_000, 0), time.Minute,
	)
	require.NoError(t, err)
	require.True(t, ok)

	sess, err := env.sessions.Get(t.Context(), env.sessionID)
	require.NoError(t, err)
	sess.SummaryMessageID = canonical[0].ID
	_, err = env.sessions.Save(t.Context(), sess)
	require.NoError(t, err)

	activated, err := env.store.activate(t.Context(), claimed, regressionSummary(candidate, "summary"), time.Unix(2_001, 0))
	require.False(t, activated)
	require.Error(t, err)
	require.True(t, IsRecoverable(err))
}

func TestClaimRejectsHistoryFromBeforeCurrentSummaryBoundary(t *testing.T) {
	t.Parallel()

	env := newTestEnvironment(t, nil, enabledOptions())
	before := env.createBatch(t, strings.Repeat("before boundary", 300))
	boundary, err := env.messages.Create(t.Context(), env.sessionID, message.CreateMessageParams{
		Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "summary"}}, IsSummaryMessage: true,
	})
	require.NoError(t, err)
	after := env.createBatch(t, strings.Repeat("after boundary", 300))
	staleHistory := append(append([]message.Message(nil), before...), boundary, after[0], after[1])
	candidate := scanCandidates(staleHistory)[0]

	sess, err := env.sessions.Get(t.Context(), env.sessionID)
	require.NoError(t, err)
	sess.SummaryMessageID = boundary.ID
	_, err = env.sessions.Save(t.Context(), sess)
	require.NoError(t, err)

	_, ok, err := env.store.claimForHistory(
		t.Context(), candidate, env.module.Options().ConfigurationID, staleHistory,
		time.Unix(2_500, 0), time.Minute,
	)
	require.ErrorContains(t, err, "summary-bounded history")
	require.False(t, ok)
}

func TestStaleClaimCanBeReclaimedForUnchangedSource(t *testing.T) {
	t.Parallel()

	summarizer := &appendOnceSummarizer{}
	env := newTestEnvironment(t, summarizer, enabledOptions())
	summarizer.env = env
	canonical := env.createBatch(t, strings.Repeat("canonical", 300))

	projected, err := env.module.Project(t.Context(), env.sessionID, canonical)
	require.NoError(t, err)
	require.Equal(t, canonical, projected)

	current, err := env.messages.List(t.Context(), env.sessionID)
	require.NoError(t, err)
	projected, err = env.module.Project(t.Context(), env.sessionID, current)
	require.NoError(t, err)
	require.Equal(t, 2, summarizer.calls)
	require.Contains(t, projected[1].ToolResults()[0].Content, "[Context projection]")
}

type appendOnceSummarizer struct {
	env   *testEnvironment
	calls int
}

func (s *appendOnceSummarizer) Summarize(ctx context.Context, candidate Candidate) (Summary, error) {
	s.calls++
	if s.calls == 1 {
		_, err := s.env.messages.Create(ctx, s.env.sessionID, message.CreateMessageParams{
			Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "concurrent"}},
		})
		if err != nil {
			return Summary{}, err
		}
	}
	return regressionSummary(candidate, "summary"), nil
}

func TestConfigurationIdentityControlsSuppressionButNotActiveRendering(t *testing.T) {
	t.Parallel()

	failing := &fixedSummarizer{err: TerminalSummaryError(context.Canceled)}
	env := newTestEnvironment(t, failing, enabledOptions())
	canonical := env.createBatch(t, strings.Repeat("canonical", 300))

	_, err := env.module.Project(t.Context(), env.sessionID, canonical)
	require.NoError(t, err)
	_, err = env.module.Project(t.Context(), env.sessionID, canonical)
	require.NoError(t, err)
	require.Equal(t, 1, failing.calls, "the same configuration remains suppressed")

	changed := enabledOptions()
	changed.ConfigurationID = ConfigurationID(changed, "new-model-or-credential")
	env.module.UpdateOptions(changed)
	_, err = env.module.Project(t.Context(), env.sessionID, canonical)
	require.NoError(t, err)
	require.Equal(t, 2, failing.calls, "a changed configuration gets a fresh durable attempt")

	summarizer := &fixedSummarizer{summary: Summary{Text: "active summary"}}
	activeEnv := newTestEnvironment(t, summarizer, enabledOptions())
	activeCanonical := activeEnv.createBatch(t, strings.Repeat("canonical", 300))
	projected, err := activeEnv.module.Project(t.Context(), activeEnv.sessionID, activeCanonical)
	require.NoError(t, err)
	require.Contains(t, projected[1].ToolResults()[0].Content, "[Context projection]")

	restartOptions := changed
	restartOptions.Enabled = false
	restarted := New(activeEnv.store, summarizer, restartOptions)
	projected, err = restarted.renderActive(t.Context(), activeEnv.sessionID, activeCanonical, map[string]struct{}{})
	require.NoError(t, err)
	require.Contains(t, projected[1].ToolResults()[0].Content, "[Context projection]")
	require.Equal(t, 1, summarizer.calls, "active rendering survives a configuration restart")
}

func TestModuleUpdateOptionsIsConcurrentSafe(t *testing.T) {
	t.Parallel()

	module := New(nil, nil, Options{})
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iteration := range 1_000 {
				options := Options{
					Enabled:           (worker+iteration)%2 == 0,
					MinBatchChars:     worker + iteration,
					KeepRecentBatches: iteration % 5,
					SummarizerTimeout: time.Duration(iteration+1) * time.Millisecond,
				}
				options.ConfigurationID = ConfigurationID(options, "concurrent")
				module.UpdateOptions(options)
				_ = module.Options()
			}
		}()
	}
	wg.Wait()
}
