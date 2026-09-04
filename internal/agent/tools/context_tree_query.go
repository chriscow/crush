package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/condense"
)

const (
	ContextTreeQueryToolName = "context_tree_query"

	contextTreeQueryDefaultLimit = 12_000
	contextTreeQueryMaxLimit     = 24_000
)

//go:embed context_tree_query.md
var contextTreeQueryDescription string

type contextQuery interface {
	Query(ctx context.Context, sessionID string, query condense.Query) (condense.Result, error)
}

// ContextTreeQueryParams identifies a projected tool result and an optional
// Unicode code-point page within it.
type ContextTreeQueryParams struct {
	Ref    string `json:"ref" description:"The projected result reference to recover, in the form t followed by a positive base-10 integer (for example, t12)."`
	Offset int    `json:"offset,omitempty" description:"The zero-based Unicode code-point offset. Defaults to 0."`
	Limit  *int   `json:"limit,omitempty" description:"The maximum number of Unicode code points to return. Defaults to 12000 and is capped at 24000."`
}

// ContextTreeQueryResult is the stable version-1 recovery response.
type ContextTreeQueryResult struct {
	Status     string `json:"status"`
	Ref        string `json:"ref"`
	ToolName   string `json:"tool_name,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
	Offset     int    `json:"offset,omitempty"`
	Returned   int    `json:"returned,omitempty"`
	Total      int    `json:"total,omitempty"`
	NextOffset *int   `json:"next_offset,omitempty"`
	Complete   bool   `json:"complete,omitempty"`
	Content    string `json:"content,omitempty"`
}

// NewContextTreeQueryTool creates the session-bound projected-result recovery
// tool.
func NewContextTreeQueryTool(query contextQuery) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ContextTreeQueryToolName,
		contextTreeQueryDescription,
		func(ctx context.Context, params ContextTreeQueryParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session ID is required for context tree queries")
			}
			if params.Offset < 0 {
				return fantasy.NewTextErrorResponse("offset must be non-negative"), nil
			}

			limit := contextTreeQueryDefaultLimit
			if params.Limit != nil {
				if *params.Limit < 1 {
					return fantasy.NewTextErrorResponse("limit must be at least 1"), nil
				}
				limit = min(*params.Limit, contextTreeQueryMaxLimit)
			}
			if !validContextTreeRef(params.Ref) {
				return contextTreeQueryResponse(ContextTreeQueryResult{
					Status: "not_found",
					Ref:    params.Ref,
				})
			}

			result, err := query.Query(ctx, sessionID, condense.Query{
				Ref:    params.Ref,
				Offset: params.Offset,
				Limit:  &limit,
			})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}

			switch result.Status {
			case "not_found":
				return contextTreeQueryResponse(ContextTreeQueryResult{
					Status: "not_found",
					Ref:    params.Ref,
				})
			case "ok":
				return contextTreeQueryResponse(ContextTreeQueryResult{
					Status:     "ok",
					Ref:        params.Ref,
					ToolName:   result.ToolName,
					ToolCallID: result.ToolCallID,
					IsError:    result.IsError,
					Offset:     result.Offset,
					Returned:   result.Returned,
					Total:      result.Total,
					NextOffset: result.NextOffset,
					Complete:   result.Complete,
					Content:    result.Content,
				})
			default:
				return fantasy.ToolResponse{}, fmt.Errorf("unsupported context tree query status %q", result.Status)
			}
		},
	)
}

func contextTreeQueryResponse(result ContextTreeQueryResult) (fantasy.ToolResponse, error) {
	data, err := json.Marshal(result)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("encode context tree query result: %w", err)
	}
	return fantasy.NewTextResponse(string(data)), nil
}

func validContextTreeRef(ref string) bool {
	if len(ref) < 2 || ref[0] != 't' || ref[1] == '0' {
		return false
	}
	for _, char := range ref[1:] {
		if char < '0' || char > '9' {
			return false
		}
	}
	number, err := strconv.ParseInt(ref[1:], 10, 64)
	return err == nil && number > 0
}
