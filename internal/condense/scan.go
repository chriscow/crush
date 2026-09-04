package condense

import (
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/message"
)

const recoveryToolName = "context_tree_query"

type callRecord struct {
	call    message.ToolCall
	ordinal int
}

func scanCandidates(messages []message.Message) []Candidate {
	candidates := make([]Candidate, 0)
	for index := 0; index < len(messages); index++ {
		assistant := messages[index]
		if assistant.Role != message.Assistant {
			continue
		}

		candidate, next, ok := scanCandidate(messages, index)
		if !ok {
			continue
		}
		candidates = append(candidates, candidate)
		index = next - 1
	}
	return candidates
}

func scanCandidate(messages []message.Message, assistantIndex int) (Candidate, int, bool) {
	assistant := messages[assistantIndex]
	calls, ok := scanAssistant(assistant)
	if !ok {
		return Candidate{}, assistantIndex + 1, false
	}

	candidate := Candidate{
		SessionID:      assistant.SessionID,
		Assistant:      assistant,
		FirstMessageID: assistant.ID,
		LastMessageID:  assistant.ID,
	}
	for ordinal, part := range assistant.Parts {
		partHash, err := sourcePartHash(assistant, ordinal, part)
		if err != nil {
			return Candidate{}, assistantIndex + 1, false
		}
		candidate.Sources = append(candidate.Sources, Source{
			MessageID: assistant.ID, PartOrdinal: ordinal, PartKind: partKind(part), SourceHash: partHash,
		})
	}

	matched := make(map[string]struct{}, len(calls))
	next := assistantIndex + 1
	for ; next < len(messages) && messages[next].Role == message.Tool; next++ {
		toolMessage := messages[next]
		if toolMessage.SessionID != assistant.SessionID {
			return Candidate{}, next, false
		}
		candidate.ToolMessages = append(candidate.ToolMessages, toolMessage)
		candidate.LastMessageID = toolMessage.ID

		hasResult := false
		for ordinal, part := range toolMessage.Parts {
			switch value := part.(type) {
			case message.ToolResult:
				hasResult = true
				call, exists := calls[value.ToolCallID]
				if !exists || call.call.Name != value.Name || value.Data != "" || value.MIMEType != "" || value.Name == recoveryToolName {
					return Candidate{}, next + 1, false
				}
				if _, duplicate := matched[value.ToolCallID]; duplicate {
					return Candidate{}, next + 1, false
				}
				matched[value.ToolCallID] = struct{}{}
				item := Item{
					MessageID: toolMessage.ID, PartOrdinal: ordinal, ToolCallID: value.ToolCallID,
					ToolName: value.Name, Result: value,
				}
				item.SourceHash = itemSourceHash(candidate.SessionID, item)
				candidate.RawChars += utf8.RuneCountInString(item.Result.Content)
				candidate.Items = append(candidate.Items, item)
			case message.Finish:
			default:
				return Candidate{}, next + 1, false
			}
		}
		if !hasResult {
			return Candidate{}, next + 1, false
		}
	}

	if len(matched) != len(calls) || len(candidate.ToolMessages) == 0 {
		return Candidate{}, next, false
	}

	for _, toolMessage := range candidate.ToolMessages {
		for ordinal, part := range toolMessage.Parts {
			partHash, hashErr := sourcePartHash(toolMessage, ordinal, part)
			if hashErr != nil {
				return Candidate{}, next, false
			}
			candidate.Sources = append(candidate.Sources, Source{
				MessageID: toolMessage.ID, PartOrdinal: ordinal, PartKind: partKind(part), SourceHash: partHash,
			})
		}
	}

	candidate.BatchKey = batchKey(candidate)
	var err error
	candidate.SourceHash, err = candidateSourceHash(candidate)
	if err != nil {
		return Candidate{}, next, false
	}
	return candidate, next, true
}

func partKind(part message.ContentPart) string {
	switch part.(type) {
	case message.ReasoningContent:
		return "reasoning"
	case message.TextContent:
		return "text"
	case message.ToolCall:
		return "tool_call"
	case message.ToolResult:
		return "tool_result"
	case message.Finish:
		return "finish"
	default:
		return "unsupported"
	}
}

func scanAssistant(assistant message.Message) (map[string]callRecord, bool) {
	if assistant.ID == "" || assistant.SessionID == "" {
		return nil, false
	}
	calls := make(map[string]callRecord)
	finishCount := 0
	for ordinal, part := range assistant.Parts {
		switch value := part.(type) {
		case message.ReasoningContent, message.TextContent:
		case message.Finish:
			finishCount++
			if finishCount != 1 || (value.Reason != message.FinishReasonToolUse && value.Reason != message.FinishReasonEndTurn) {
				return nil, false
			}
		case message.ToolCall:
			if value.ID == "" || value.Name == "" || !value.Finished || value.Name == recoveryToolName {
				return nil, false
			}
			if _, duplicate := calls[value.ID]; duplicate {
				return nil, false
			}
			calls[value.ID] = callRecord{call: value, ordinal: ordinal}
		default:
			return nil, false
		}
	}
	return calls, len(calls) > 0 && finishCount == 1
}
