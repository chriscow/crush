package agent

import (
	"context"
	"errors"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/condense"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

type projectionRecorder struct {
	mu           sync.Mutex
	events       *[]string
	calls        int
	session      string
	messages     []message.Message
	result       []message.Message
	err          error
	beforeReturn func()
	accountUsage func(context.Context, string) error
	usageOnce    sync.Once
}

func (p *projectionRecorder) Project(ctx context.Context, sessionID string, messages []message.Message) ([]message.Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.session = sessionID
	p.messages = append([]message.Message(nil), messages...)
	if p.events != nil {
		*p.events = append(*p.events, "project")
	}
	if p.beforeReturn != nil {
		p.beforeReturn()
	}
	if p.result != nil {
		return p.result, p.err
	}
	return messages, p.err
}

func (p *projectionRecorder) AccountUsage(ctx context.Context, sessionID string) error {
	if p.accountUsage == nil {
		return nil
	}
	p.usageOnce.Do(func() { _ = p.accountUsage(ctx, sessionID) })
	return nil
}

type orderedMessageService struct {
	message.Service
	mu        sync.Mutex
	events    []string
	flushErr  error
	projected *bool
}

func (s *orderedMessageService) FlushAll(ctx context.Context) error {
	s.mu.Lock()
	s.events = append(s.events, "flush")
	s.mu.Unlock()
	if s.flushErr != nil {
		return s.flushErr
	}
	return s.Service.FlushAll(ctx)
}

func (s *orderedMessageService) List(ctx context.Context, sessionID string) ([]message.Message, error) {
	s.mu.Lock()
	s.events = append(s.events, "list")
	s.mu.Unlock()
	return s.Service.List(ctx, sessionID)
}

func (s *orderedMessageService) Create(ctx context.Context, sessionID string, params message.CreateMessageParams) (message.Message, error) {
	if params.IsSummaryMessage && s.projected != nil && *s.projected {
		return message.Message{}, errors.New("manual summarization unexpectedly invoked context projection")
	}
	if params.Role == message.User {
		s.mu.Lock()
		s.events = append(s.events, "create_user")
		s.mu.Unlock()
	}
	return s.Service.Create(ctx, sessionID, params)
}

type historyCaptureModel struct {
	finishStreamModel
	mu    sync.Mutex
	call  fantasy.Call
	usage fantasy.Usage
}

func (m *historyCaptureModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	m.call = call
	m.mu.Unlock()
	stream, err := m.finishStreamModel.Stream(context.Background(), call)
	if err != nil || usageIsZero(m.usage) {
		return stream, err
	}
	return func(yield func(fantasy.StreamPart) bool) {
		stream(func(part fantasy.StreamPart) bool {
			if part.Type == fantasy.StreamPartTypeFinish {
				part.Usage = m.usage
			}
			return yield(part)
		})
	}, nil
}

func TestRunProjectsSummaryBoundedHistoryBeforeUserPersistence(t *testing.T) {
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "existing")
	require.NoError(t, err)

	old, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "outside summary boundary"}},
	})
	require.NoError(t, err)
	boundary, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "canonical summary"}}, IsSummaryMessage: true,
	})
	require.NoError(t, err)
	inside, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "inside boundary"}},
	})
	require.NoError(t, err)
	sess.SummaryMessageID = boundary.ID
	_, err = env.sessions.Save(t.Context(), sess)
	require.NoError(t, err)

	ordered := &orderedMessageService{Service: env.messages}
	projectedBoundary := boundary
	projectedBoundary.Role = message.User
	projectedBoundary.Parts = []message.ContentPart{message.TextContent{Text: "provider-only projected summary"}}
	projector := &projectionRecorder{
		events: &ordered.events,
		result: []message.Message{projectedBoundary, inside},
	}
	model := &historyCaptureModel{finishStreamModel: finishStreamModel{text: "done"}}
	agent := NewSessionAgent(SessionAgentOptions{
		LargeModel: Model{Model: model}, SmallModel: Model{Model: model},
		SystemPrompt: "system", Sessions: env.sessions, Messages: ordered, Projector: projector, IsYolo: true,
	})

	_, err = agent.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "new prompt"})
	require.NoError(t, err)

	ordered.mu.Lock()
	events := append([]string(nil), ordered.events...)
	ordered.mu.Unlock()
	require.GreaterOrEqual(t, len(events), 4)
	require.Equal(t, []string{"flush", "list", "project", "create_user"}, events[:4])
	require.Equal(t, sess.ID, projector.session)
	require.NotEmpty(t, projector.messages)
	require.Equal(t, boundary.ID, projector.messages[0].ID)
	require.Equal(t, message.User, projector.messages[0].Role, "existing summary-boundary role rewrite remains canonical")
	for _, msg := range projector.messages {
		require.NotEqual(t, old.ID, msg.ID)
		require.NotEqual(t, "new prompt", msg.Content().Text, "projector must receive the immutable pre-user snapshot")
	}

	model.mu.Lock()
	providerCall := model.call
	model.mu.Unlock()
	var historyText []string
	for _, msg := range providerCall.Prompt {
		for _, part := range msg.Content {
			if text, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
				historyText = append(historyText, text.Text)
			}
		}
	}
	require.Contains(t, historyText, "provider-only projected summary")
	require.NotContains(t, historyText, "canonical summary")
}

func TestRunAddsProjectionUsageAfterMainModelSessionSave(t *testing.T) {
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "usage")
	require.NoError(t, err)

	model := &historyCaptureModel{
		finishStreamModel: finishStreamModel{text: "done"},
		usage:             fantasy.Usage{InputTokens: 11, OutputTokens: 7, TotalTokens: 18},
	}
	projector := &projectionRecorder{accountUsage: func(ctx context.Context, sessionID string) error {
		return env.sessions.AddUsage(ctx, sessionID, 3, 2, 0.25)
	}}
	agent := NewSessionAgent(SessionAgentOptions{
		LargeModel: Model{Model: model}, SmallModel: Model{Model: model},
		SystemPrompt: "system", Sessions: env.sessions, Messages: env.messages, Projector: projector, IsYolo: true,
	})

	_, err = agent.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "new prompt"})
	require.NoError(t, err)

	updated, err := env.sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, int64(14), updated.PromptTokens)
	require.Equal(t, int64(9), updated.CompletionTokens)
	require.Equal(t, 0.25, updated.Cost)
}

func TestProjectedToolBatchPreservesPreparePromptPairing(t *testing.T) {
	agent := NewSessionAgent(SessionAgentOptions{IsSubAgent: true}).(*sessionAgent)
	messages := []message.Message{
		{
			ID: "assistant", SessionID: "session", Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "Checking both files"},
				message.ToolCall{ID: "call-1", Name: "view", Input: `{"path":"one"}`, Finished: true},
				message.ToolCall{ID: "call-2", Name: "grep", Input: `{"pattern":"two"}`, Finished: true},
				message.Finish{Reason: message.FinishReasonToolUse},
			},
		},
		{
			ID: "tools", SessionID: "session", Role: message.Tool,
			Parts: []message.ContentPart{
				message.ToolResult{ToolCallID: "call-1", Name: "view", Content: "[Context projection]\nSummary: files checked\nOriginal result: [Projected as t1.]"},
				message.ToolResult{ToolCallID: "call-2", Name: "grep", Content: "[Projected as t2 in the preceding context summary.]"},
				message.Finish{Reason: message.FinishReasonEndTurn},
			},
		},
	}

	history, _ := agent.preparePrompt(messages, false)
	calls := make(map[string]int)
	results := make(map[string]int)
	for _, msg := range history {
		for _, part := range msg.Content {
			if call, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
				calls[call.ToolCallID]++
			}
			if result, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				results[result.ToolCallID]++
			}
		}
	}
	require.Equal(t, map[string]int{"call-1": 1, "call-2": 1}, calls)
	require.Equal(t, calls, results)
}

func TestRunFlushFailureAbortsBeforeHistoryAndUserPersistence(t *testing.T) {
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "existing")
	require.NoError(t, err)
	ordered := &orderedMessageService{Service: env.messages, flushErr: errors.New("flush failed")}
	projector := &projectionRecorder{}
	model := &historyCaptureModel{finishStreamModel: finishStreamModel{text: "done"}}
	agent := NewSessionAgent(SessionAgentOptions{
		LargeModel: Model{Model: model}, SmallModel: Model{Model: model},
		SystemPrompt: "system", Sessions: env.sessions, Messages: ordered, Projector: projector, IsYolo: true,
	})

	_, err = agent.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "new prompt"})
	require.ErrorContains(t, err, "failed to flush messages before context projection")
	require.Zero(t, projector.calls)
	ordered.mu.Lock()
	events := append([]string(nil), ordered.events...)
	ordered.mu.Unlock()
	require.Equal(t, []string{"flush"}, events)
	listed, listErr := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, listErr)
	require.Empty(t, listed)
}

func TestRunProjectionRecoverableFallbackAndFatalAbort(t *testing.T) {
	tests := []struct {
		name       string
		projectErr error
		cancel     bool
		wantErr    bool
	}{
		{
			name: "recoverable uses raw history",
			projectErr: &condense.ProjectionError{
				Class: condense.ErrorClassStore, Recoverable: true, Err: errors.New("temporary store failure"),
			},
		},
		{
			name: "typed fatal aborts",
			projectErr: &condense.ProjectionError{
				Class: condense.ErrorClassStore, Recoverable: false, Err: errors.New("fatal projection failure"),
			},
			wantErr: true,
		},
		{name: "parent cancellation aborts", cancel: true, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := testEnv(t)
			sess, err := env.sessions.Create(t.Context(), "existing")
			require.NoError(t, err)
			_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
				Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "canonical"}},
			})
			require.NoError(t, err)

			model := &historyCaptureModel{finishStreamModel: finishStreamModel{text: "done"}}
			projector := &projectionRecorder{err: test.projectErr}
			runCtx := t.Context()
			if test.cancel {
				var cancel context.CancelCauseFunc
				runCtx, cancel = context.WithCancelCause(runCtx)
				projector.err = &condense.ProjectionError{Class: condense.ErrorClassSummarizer, Recoverable: true, Err: context.Canceled}
				projector.beforeReturn = func() { cancel(errors.New("parent stopped")) }
			}
			agent := NewSessionAgent(SessionAgentOptions{
				LargeModel: Model{Model: model}, SmallModel: Model{Model: model},
				SystemPrompt: "system", Sessions: env.sessions, Messages: env.messages, Projector: projector, IsYolo: true,
			})

			_, err = agent.Run(runCtx, SessionAgentCall{SessionID: sess.ID, Prompt: "new prompt"})
			if test.wantErr {
				require.Error(t, err)
				listed, listErr := env.messages.List(t.Context(), sess.ID)
				require.NoError(t, listErr)
				require.Len(t, listed, 1, "fatal projection failure must abort before user persistence")
				return
			}
			require.NoError(t, err)
			listed, listErr := env.messages.List(t.Context(), sess.ID)
			require.NoError(t, listErr)
			require.GreaterOrEqual(t, len(listed), 3)
		})
	}
}

func TestSummarizeDoesNotInvokeContextProjection(t *testing.T) {
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "existing")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "canonical"}},
	})
	require.NoError(t, err)

	projected := false
	projector := &projectionRecorder{beforeReturn: func() { projected = true }}
	ordered := &orderedMessageService{Service: env.messages, projected: &projected}
	model := &finishStreamModel{text: "summary"}
	agent := NewSessionAgent(SessionAgentOptions{
		LargeModel: Model{Model: model}, SmallModel: Model{Model: model},
		SystemPrompt: "system", Sessions: env.sessions, Messages: ordered, Projector: projector, IsYolo: true,
	})

	err = agent.Summarize(t.Context(), sess.ID, nil, nil)
	require.NoError(t, err)
	require.Zero(t, projector.calls)
	require.False(t, projected)
}

func TestRunWithoutProjectorDoesNotAddPreReadFlush(t *testing.T) {
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "existing")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "canonical"}},
	})
	require.NoError(t, err)

	ordered := &orderedMessageService{Service: env.messages}
	model := &historyCaptureModel{finishStreamModel: finishStreamModel{text: "done"}}
	agent := NewSessionAgent(SessionAgentOptions{
		LargeModel: Model{Model: model}, SmallModel: Model{Model: model},
		SystemPrompt: "system", Sessions: env.sessions, Messages: ordered, IsYolo: true,
	})
	_, err = agent.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "new prompt"})
	require.NoError(t, err)

	ordered.mu.Lock()
	events := append([]string(nil), ordered.events...)
	ordered.mu.Unlock()
	require.NotEmpty(t, events)
	require.Equal(t, "list", events[0], "nil projector preserves the historical no-pre-read-flush path")
}
