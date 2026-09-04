package condense

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
)

type activeProjection struct {
	node    db.ContextProjectionNode
	sources []db.ContextProjectionSource
	items   []db.ContextProjectionItem
}

type projectionReplacement struct {
	content string
}

func renderProjection(summary string, items []db.ContextProjectionItem) (map[sourceIdentity]projectionReplacement, int, error) {
	replacements := make(map[sourceIdentity]projectionReplacement, len(items))
	if len(items) == 0 {
		return replacements, 0, nil
	}

	ownedRefs := make(map[int64]struct{}, len(items))
	for _, item := range items {
		if item.RefNumber <= 0 {
			return nil, 0, fmt.Errorf("projection item has invalid ref")
		}
		if _, duplicate := ownedRefs[item.RefNumber]; duplicate {
			return nil, 0, fmt.Errorf("projection items have duplicate ref")
		}
		ownedRefs[item.RefNumber] = struct{}{}
	}

	var first strings.Builder
	first.WriteString("[Context projection]\nSummary: ")
	first.WriteString(normalizeModelText(summary, maxSummaryChars))
	first.WriteString("\nReferences:\n")
	for _, item := range items {
		fmt.Fprintf(&first, "- t%d (%s): %s\n", item.RefNumber, normalizeModelText(item.ToolName, maxDescriptionChars), normalizeModelText(item.Description, maxDescriptionChars))
	}
	fmt.Fprintf(&first, "Original result: [Projected as t%d. Use context_tree_query to recover it.]", items[0].RefNumber)

	firstContent := first.String()
	if err := requireOwnedVisibleRefs(firstContent, ownedRefs); err != nil {
		return nil, 0, err
	}
	total := utf8.RuneCountInString(firstContent)
	replacements[sourceIdentity{items[0].SourceMessageID, int(items[0].SourcePartOrdinal)}] = projectionReplacement{content: firstContent}
	for _, item := range items[1:] {
		content := fmt.Sprintf("[Projected as t%d in the preceding context summary. Use context_tree_query to recover the original result.]", item.RefNumber)
		if err := requireOwnedVisibleRefs(content, ownedRefs); err != nil {
			return nil, 0, err
		}
		total += utf8.RuneCountInString(content)
		replacements[sourceIdentity{item.SourceMessageID, int(item.SourcePartOrdinal)}] = projectionReplacement{content: content}
	}
	return replacements, total, nil
}

func requireOwnedVisibleRefs(content string, owned map[int64]struct{}) error {
	for offset := 0; offset < len(content); offset++ {
		if content[offset] != 't' || offset+1 >= len(content) || content[offset+1] < '1' || content[offset+1] > '9' || (offset > 0 && content[offset-1] == '\\') {
			continue
		}
		end := offset + 2
		for end < len(content) && content[end] >= '0' && content[end] <= '9' {
			end++
		}
		number, err := strconv.ParseInt(content[offset+1:end], 10, 64)
		if err != nil {
			return fmt.Errorf("projection rendered invalid visible ref")
		}
		if _, ok := owned[number]; !ok {
			return fmt.Errorf("projection rendered unowned visible ref")
		}
		offset = end - 1
	}
	return nil
}

func applyProjections(messages []message.Message, active []activeProjection) []message.Message {
	if len(messages) == 0 || len(active) == 0 {
		return messages
	}

	replacements := make(map[sourceIdentity]projectionReplacement)
	ownedRefs := make(map[int64]sourceIdentity)
	for _, projection := range active {
		if !projectionValid(messages, projection) {
			continue
		}
		for _, item := range projection.items {
			identity := sourceIdentity{item.SourceMessageID, int(item.SourcePartOrdinal)}
			if owner, duplicate := ownedRefs[item.RefNumber]; duplicate && owner != identity {
				return messages
			}
			ownedRefs[item.RefNumber] = identity
		}
		rendered, _, err := renderProjection(projection.node.Summary, projection.items)
		if err != nil {
			continue
		}
		for identity, replacement := range rendered {
			replacements[identity] = replacement
		}
	}
	if len(replacements) == 0 {
		return messages
	}

	projected := messages
	clonedSlice := false
	for messageIndex := range messages {
		var cloned *message.Message
		for partOrdinal, part := range messages[messageIndex].Parts {
			result, ok := part.(message.ToolResult)
			if !ok {
				continue
			}
			replacement, ok := replacements[sourceIdentity{messages[messageIndex].ID, partOrdinal}]
			if !ok {
				continue
			}
			if !clonedSlice {
				projected = append([]message.Message(nil), messages...)
				clonedSlice = true
			}
			if cloned == nil {
				value := messages[messageIndex].Clone()
				cloned = &value
				projected[messageIndex] = value
			}
			result.Content = replacement.content
			cloned.Parts[partOrdinal] = result
		}
	}
	return projected
}

func projectionValid(messages []message.Message, projection activeProjection) bool {
	if len(projection.items) == 0 {
		return false
	}
	if len(projection.sources) == 0 {
		return false
	}
	if projection.node.SessionID == "" || projection.node.SourceHash == "" {
		return false
	}

	byID := make(map[string]message.Message, len(messages))
	messageOrder := make(map[string]int, len(messages))
	for index, msg := range messages {
		if msg.SessionID != projection.node.SessionID {
			continue
		}
		if _, duplicate := byID[msg.ID]; duplicate {
			return false
		}
		byID[msg.ID] = msg
		messageOrder[msg.ID] = index
	}

	seenMessages := make(map[string]struct{})
	lastMessageIndex := -1
	for ordinal, source := range projection.sources {
		if source.NodeID != projection.node.ID || source.SessionID != projection.node.SessionID || source.Ordinal != int64(ordinal) {
			return false
		}
		msg, ok := byID[source.SourceMessageID]
		if !ok || source.SourcePartOrdinal < 0 || source.SourcePartOrdinal >= int64(len(msg.Parts)) {
			return false
		}
		part := msg.Parts[source.SourcePartOrdinal]
		partHash, err := sourcePartHash(msg, int(source.SourcePartOrdinal), part)
		if err != nil || partKind(part) != source.PartKind || partHash != source.SourceHash {
			return false
		}
		if _, seen := seenMessages[msg.ID]; !seen {
			index := messageOrder[msg.ID]
			if index <= lastMessageIndex {
				return false
			}
			seenMessages[msg.ID] = struct{}{}
			lastMessageIndex = index
		}
	}

	var candidate Candidate
	matched := false
	for _, current := range scanCandidates(messages) {
		if current.SessionID == projection.node.SessionID && current.FirstMessageID == projection.node.FirstMessageID && current.LastMessageID == projection.node.LastMessageID && current.BatchKey == projection.node.BatchKey && current.SourceHash == projection.node.SourceHash {
			candidate = current
			matched = true
			break
		}
	}
	if !matched || len(candidate.Sources) != len(projection.sources) || len(candidate.Items) != len(projection.items) {
		return false
	}
	for index, source := range candidate.Sources {
		stored := projection.sources[index]
		if source.MessageID != stored.SourceMessageID || int64(source.PartOrdinal) != stored.SourcePartOrdinal || source.PartKind != stored.PartKind || source.SourceHash != stored.SourceHash {
			return false
		}
	}
	ownedRefs := make(map[int64]struct{}, len(projection.items))
	for index, item := range candidate.Items {
		stored := projection.items[index]
		if stored.NodeID != projection.node.ID || stored.SessionID != projection.node.SessionID || stored.Ordinal != int64(index) || stored.AlgorithmVersion != AlgorithmVersion || stored.RefNumber <= 0 || item.MessageID != stored.SourceMessageID || int64(item.PartOrdinal) != stored.SourcePartOrdinal || item.ToolCallID != stored.ToolCallID || item.ToolName != stored.ToolName || item.SourceHash != stored.SourceHash {
			return false
		}
		if _, duplicate := ownedRefs[stored.RefNumber]; duplicate {
			return false
		}
		ownedRefs[stored.RefNumber] = struct{}{}
	}
	return true
}

type sourceIdentity struct {
	messageID   string
	partOrdinal int
}

func normalizeModelText(value string, maxRunes int) string {
	if value == "" || maxRunes <= 0 {
		return ""
	}

	const (
		contextMarker    = "[Context projection]"
		projectedMarker  = "[Projected as"
		contextLiteral   = "[Model text: Context projection]"
		projectedLiteral = "[Model text: Projected as"
	)

	var normalized strings.Builder
	written := 0
	pendingSpace := false
	writeRune := func(r rune) bool {
		if written >= maxRunes {
			return false
		}
		normalized.WriteRune(r)
		written++
		return true
	}
	writeString := func(value string) bool {
		for _, r := range value {
			if !writeRune(r) {
				return false
			}
		}
		return true
	}

	for byteOffset := 0; byteOffset < len(value); {
		r, size := utf8.DecodeRuneInString(value[byteOffset:])
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			pendingSpace = written > 0
			byteOffset += size
			continue
		}
		if pendingSpace {
			if !writeRune(' ') {
				break
			}
			pendingSpace = false
		}
		switch {
		case strings.HasPrefix(value[byteOffset:], contextMarker):
			if !writeString(contextLiteral) {
				return normalized.String()
			}
			byteOffset += len(contextMarker)
			continue
		case strings.HasPrefix(value[byteOffset:], projectedMarker):
			if !writeString(projectedLiteral) {
				return normalized.String()
			}
			byteOffset += len(projectedMarker)
			continue
		}
		if r == 't' && byteOffset+size < len(value) && value[byteOffset+size] >= '0' && value[byteOffset+size] <= '9' {
			if !writeRune(r) || !writeRune('\\') {
				break
			}
			byteOffset += size
			continue
		}
		if !writeRune(r) {
			break
		}
		byteOffset += size
	}
	return normalized.String()
}

func refString(number int64) string {
	return "t" + strconv.FormatInt(number, 10)
}
