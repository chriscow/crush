package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/condense"
	"github.com/stretchr/testify/require"
)

type contextQueryCall struct {
	sessionID string
	query     condense.Query
}

type fakeContextQuery struct {
	calls  []contextQueryCall
	result condense.Result
	err    error
}

func (f *fakeContextQuery) Query(_ context.Context, sessionID string, query condense.Query) (condense.Result, error) {
	f.calls = append(f.calls, contextQueryCall{sessionID: sessionID, query: query})
	return f.result, f.err
}

func runContextTreeQueryTool(
	t *testing.T,
	tool fantasy.AgentTool,
	ctx context.Context,
	params ContextTreeQueryParams,
) (fantasy.ToolResponse, error) {
	t.Helper()

	input, err := json.Marshal(params)
	require.NoError(t, err)
	return tool.Run(ctx, fantasy.ToolCall{
		ID:    "test-call",
		Name:  ContextTreeQueryToolName,
		Input: string(input),
	})
}

func contextTreeQueryTestContext(sessionID string) context.Context {
	return context.WithValue(context.Background(), SessionIDContextKey, sessionID)
}

func TestContextTreeQueryToolInfo(t *testing.T) {
	t.Parallel()

	tool := NewContextTreeQueryTool(&fakeContextQuery{})
	info := tool.Info()
	require.Equal(t, ContextTreeQueryToolName, info.Name)
	require.NotEmpty(t, info.Description)
	require.Equal(t, []string{"ref"}, info.Required)
	require.Contains(t, info.Parameters, "ref")
	require.Contains(t, info.Parameters, "offset")
	require.Contains(t, info.Parameters, "limit")
	require.NotContains(t, info.Parameters, "session")
	require.NotContains(t, info.Parameters, "session_id")
}

func TestContextTreeQueryUsesTrustedSessionAndDefaultsLimit(t *testing.T) {
	t.Parallel()

	query := &fakeContextQuery{result: condense.Result{
		Status:   "ok",
		Ref:      "t12",
		Complete: true,
		Content:  "original",
		Returned: 8,
		Total:    8,
	}}
	resp, err := runContextTreeQueryTool(
		t,
		NewContextTreeQueryTool(query),
		contextTreeQueryTestContext("trusted-session"),
		ContextTreeQueryParams{Ref: "t12", Offset: 3},
	)
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Len(t, query.calls, 1)
	require.Equal(t, "trusted-session", query.calls[0].sessionID)
	require.Equal(t, "t12", query.calls[0].query.Ref)
	require.Equal(t, 3, query.calls[0].query.Offset)
	require.NotNil(t, query.calls[0].query.Limit)
	require.Equal(t, contextTreeQueryDefaultLimit, *query.calls[0].query.Limit)
}

func TestContextTreeQueryRequiresTrustedSession(t *testing.T) {
	t.Parallel()

	query := &fakeContextQuery{}
	_, err := runContextTreeQueryTool(
		t,
		NewContextTreeQueryTool(query),
		context.Background(),
		ContextTreeQueryParams{Ref: "t1"},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "session ID")
	require.Empty(t, query.calls)
}

func TestContextTreeQueryStrictValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		params     ContextTreeQueryParams
		wantError  string
		wantResult string
	}{
		{name: "negative offset", params: ContextTreeQueryParams{Ref: "t1", Offset: -1}, wantError: "offset must be non-negative"},
		{name: "zero limit", params: ContextTreeQueryParams{Ref: "t1", Limit: intPointer(0)}, wantError: "limit must be at least 1"},
		{name: "negative limit", params: ContextTreeQueryParams{Ref: "t1", Limit: intPointer(-1)}, wantError: "limit must be at least 1"},
		{name: "empty ref", params: ContextTreeQueryParams{}, wantResult: `{"status":"not_found","ref":""}`},
		{name: "zero ref", params: ContextTreeQueryParams{Ref: "t0"}, wantResult: `{"status":"not_found","ref":"t0"}`},
		{name: "leading zero", params: ContextTreeQueryParams{Ref: "t01"}, wantResult: `{"status":"not_found","ref":"t01"}`},
		{name: "plus sign", params: ContextTreeQueryParams{Ref: "t+1"}, wantResult: `{"status":"not_found","ref":"t+1"}`},
		{name: "wrong prefix", params: ContextTreeQueryParams{Ref: "x1"}, wantResult: `{"status":"not_found","ref":"x1"}`},
		{name: "overflow", params: ContextTreeQueryParams{Ref: "t9223372036854775808"}, wantResult: `{"status":"not_found","ref":"t9223372036854775808"}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			query := &fakeContextQuery{}
			resp, err := runContextTreeQueryTool(
				t,
				NewContextTreeQueryTool(query),
				contextTreeQueryTestContext("session"),
				test.params,
			)
			require.NoError(t, err)
			require.Empty(t, query.calls)
			if test.wantError != "" {
				require.True(t, resp.IsError)
				require.Contains(t, resp.Content, test.wantError)
				return
			}
			require.False(t, resp.IsError)
			require.JSONEq(t, test.wantResult, resp.Content)
		})
	}
}

func TestContextTreeQueryClampsExplicitLimit(t *testing.T) {
	t.Parallel()

	query := &fakeContextQuery{result: condense.Result{Status: "not_found", Ref: "t1"}}
	resp, err := runContextTreeQueryTool(
		t,
		NewContextTreeQueryTool(query),
		contextTreeQueryTestContext("session"),
		ContextTreeQueryParams{Ref: "t1", Limit: intPointer(contextTreeQueryMaxLimit + 1)},
	)
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Len(t, query.calls, 1)
	require.Equal(t, contextTreeQueryMaxLimit, *query.calls[0].query.Limit)
}

func TestContextTreeQueryReturnsStableOKJSON(t *testing.T) {
	t.Parallel()

	nextOffset := 9
	query := &fakeContextQuery{result: condense.Result{
		Status:     "ok",
		Ref:        "t7",
		ToolName:   "bash",
		ToolCallID: "call-7",
		IsError:    true,
		Offset:     5,
		Returned:   4,
		Total:      12,
		NextOffset: &nextOffset,
		Content:    "界🙂\n\"",
	}}
	resp, err := runContextTreeQueryTool(
		t,
		NewContextTreeQueryTool(query),
		contextTreeQueryTestContext("session"),
		ContextTreeQueryParams{Ref: "t7", Offset: 5, Limit: intPointer(4)},
	)
	require.NoError(t, err)
	require.False(t, resp.IsError)

	var result ContextTreeQueryResult
	require.NoError(t, json.Unmarshal([]byte(resp.Content), &result))
	require.Equal(t, ContextTreeQueryResult{
		Status:     "ok",
		Ref:        "t7",
		ToolName:   "bash",
		ToolCallID: "call-7",
		IsError:    true,
		Offset:     5,
		Returned:   4,
		Total:      12,
		NextOffset: &nextOffset,
		Content:    "界🙂\n\"",
	}, result)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(resp.Content), &raw))
	require.Contains(t, raw, "tool_name")
	require.Contains(t, raw, "tool_call_id")
	require.Contains(t, raw, "is_error")
	require.Contains(t, raw, "next_offset")
	require.NotContains(t, raw, "ToolName")
	require.NotContains(t, raw, "complete")
}

func TestContextTreeQueryCompleteResponseOmitsNextOffset(t *testing.T) {
	t.Parallel()

	query := &fakeContextQuery{result: condense.Result{
		Status:     "ok",
		Ref:        "t2",
		ToolName:   "view",
		ToolCallID: "call-2",
		Complete:   true,
		Content:    "done",
		Returned:   4,
		Total:      4,
	}}
	resp, err := runContextTreeQueryTool(
		t,
		NewContextTreeQueryTool(query),
		contextTreeQueryTestContext("session"),
		ContextTreeQueryParams{Ref: "t2"},
	)
	require.NoError(t, err)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(resp.Content), &raw))
	require.Contains(t, raw, "complete")
	require.NotContains(t, raw, "next_offset")
}

func TestContextTreeQuerySanitizesNotFoundResult(t *testing.T) {
	t.Parallel()

	query := &fakeContextQuery{result: condense.Result{
		Status:     "not_found",
		Ref:        "different-ref",
		ToolName:   "secret-tool",
		ToolCallID: "secret-call",
		Content:    "secret-content",
	}}
	resp, err := runContextTreeQueryTool(
		t,
		NewContextTreeQueryTool(query),
		contextTreeQueryTestContext("session"),
		ContextTreeQueryParams{Ref: "t9"},
	)
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.JSONEq(t, `{"status":"not_found","ref":"t9"}`, resp.Content)
}

func TestContextTreeQueryPropagatesQueryError(t *testing.T) {
	t.Parallel()

	queryErr := errors.New("database unavailable")
	query := &fakeContextQuery{err: queryErr}
	resp, err := runContextTreeQueryTool(
		t,
		NewContextTreeQueryTool(query),
		contextTreeQueryTestContext("session"),
		ContextTreeQueryParams{Ref: "t1"},
	)
	require.ErrorIs(t, err, queryErr)
	require.Empty(t, resp.Content)
}

func TestContextTreeQueryRejectsUnknownStatus(t *testing.T) {
	t.Parallel()

	query := &fakeContextQuery{result: condense.Result{Status: "future", Ref: "t1"}}
	_, err := runContextTreeQueryTool(
		t,
		NewContextTreeQueryTool(query),
		contextTreeQueryTestContext("session"),
		ContextTreeQueryParams{Ref: "t1"},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported context tree query status")
}

func intPointer(value int) *int {
	return &value
}
