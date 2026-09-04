package condense

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/stringext"
)

// Query resolves a durable reference in the trusted session and paginates the
// exact canonical result by Unicode code points.
func (m *Module) Query(ctx context.Context, sessionID string, query Query) (Result, error) {
	result := Result{Status: "not_found", Ref: query.Ref}
	refNumber, ok := parseRef(query.Ref)
	if !ok || query.Offset < 0 || (query.Limit != nil && *query.Limit < 1) {
		return result, nil
	}
	if !m.store.available() {
		return Result{}, fmt.Errorf("context projection store is unavailable")
	}
	limit := defaultQueryLimit
	if query.Limit != nil {
		limit = min(*query.Limit, maxQueryLimit)
	}

	item, err := m.store.q.GetActiveContextProjectionItemByRef(ctx, db.GetActiveContextProjectionItemByRefParams{
		SessionID: sessionID, RefNumber: refNumber,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("load context projection ref: %w", err)
	}
	if item.AlgorithmVersion != AlgorithmVersion {
		return result, nil
	}
	row, err := m.store.q.GetMessage(ctx, item.SourceMessageID)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("load canonical source: %w", err)
	}
	if row.SessionID != sessionID || row.SessionID != item.SessionID {
		return result, nil
	}
	parts, err := message.DecodeParts([]byte(row.Parts))
	if err != nil {
		return Result{}, fmt.Errorf("decode canonical source: %w", err)
	}
	if item.SourcePartOrdinal < 0 || item.SourcePartOrdinal >= int64(len(parts)) {
		return result, nil
	}
	toolResult, ok := parts[item.SourcePartOrdinal].(message.ToolResult)
	if !ok || toolResult.ToolCallID != item.ToolCallID || toolResult.Name != item.ToolName {
		return result, nil
	}
	candidateItem := Item{
		MessageID: item.SourceMessageID, PartOrdinal: int(item.SourcePartOrdinal),
		ToolCallID: toolResult.ToolCallID, ToolName: toolResult.Name, Result: toolResult,
	}
	if itemSourceHash(sessionID, candidateItem) != item.SourceHash {
		return result, nil
	}

	content, offset, returned, total := stringext.RunePage(toolResult.Content, query.Offset, limit)
	end := offset + returned
	complete := end == total
	result = Result{
		Status: "ok", Ref: query.Ref, ToolName: toolResult.Name, ToolCallID: toolResult.ToolCallID,
		IsError: toolResult.IsError, Offset: offset, Returned: returned, Total: total,
		Complete: complete, Content: content,
	}
	if !complete {
		next := end
		result.NextOffset = &next
	}
	return result, nil
}

func parseRef(value string) (int64, bool) {
	if len(value) < 2 || value[0] != 't' || strings.HasPrefix(value[1:], "+") || value[1] == '0' {
		return 0, false
	}
	for _, r := range value[1:] {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	number, err := strconv.ParseInt(value[1:], 10, 64)
	return number, err == nil && number > 0
}
