package condense

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"

	"github.com/charmbracelet/crush/internal/message"
)

type hashEncoder struct {
	h hash.Hash
}

func newHashEncoder(domain string) *hashEncoder {
	e := &hashEncoder{h: sha256.New()}
	e.field("domain", domain)
	return e
}

func (e *hashEncoder) field(tag, value string) {
	e.bytes([]byte(tag))
	e.bytes([]byte(value))
}

func (e *hashEncoder) integer(tag string, value int64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	e.bytes([]byte(tag))
	e.bytes(encoded[:])
}

func (e *hashEncoder) boolean(tag string, value bool) {
	if value {
		e.field(tag, "1")
		return
	}
	e.field(tag, "0")
}

func (e *hashEncoder) bytes(value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = e.h.Write(length[:])
	_, _ = e.h.Write(value)
}

func (e *hashEncoder) sum() string {
	return hex.EncodeToString(e.h.Sum(nil))
}

func batchKey(candidate Candidate) string {
	e := newHashEncoder("crush-context-projection-batch-v1")
	e.field("assistant_message_id", candidate.Assistant.ID)
	for _, item := range candidate.Items {
		e.field("result_message_id", item.MessageID)
		e.integer("result_part_ordinal", int64(item.PartOrdinal))
	}
	return e.sum()
}

func candidateSourceHash(candidate Candidate) (string, error) {
	e := newHashEncoder("crush-context-projection-source-v1")
	e.integer("algorithm_version", AlgorithmVersion)
	if err := encodeMessage(e, candidate.Assistant); err != nil {
		return "", err
	}
	for _, toolMessage := range candidate.ToolMessages {
		if err := encodeMessage(e, toolMessage); err != nil {
			return "", err
		}
	}
	return e.sum(), nil
}

func sourcePartHash(msg message.Message, partOrdinal int, part message.ContentPart) (string, error) {
	e := newHashEncoder("crush-context-projection-part-v1")
	e.integer("algorithm_version", AlgorithmVersion)
	e.field("message_id", msg.ID)
	e.field("role", string(msg.Role))
	e.integer("part_ordinal", int64(partOrdinal))
	if err := encodePart(e, partOrdinal, part); err != nil {
		return "", err
	}
	return e.sum(), nil
}

func itemSourceHash(sessionID string, item Item) string {
	e := newHashEncoder("crush-context-projection-item-v1")
	e.integer("algorithm_version", AlgorithmVersion)
	e.field("session_id", sessionID)
	e.field("message_id", item.MessageID)
	e.integer("part_ordinal", int64(item.PartOrdinal))
	encodeToolResult(e, item.Result)
	return e.sum()
}

func encodeMessage(e *hashEncoder, msg message.Message) error {
	e.field("message_id", msg.ID)
	e.field("role", string(msg.Role))
	e.integer("part_count", int64(len(msg.Parts)))
	for ordinal, part := range msg.Parts {
		if err := encodePart(e, ordinal, part); err != nil {
			return err
		}
	}
	return nil
}

func encodePart(e *hashEncoder, ordinal int, part message.ContentPart) error {
	e.integer("part_ordinal", int64(ordinal))
	switch value := part.(type) {
	case message.ReasoningContent:
		e.field("part_type", "reasoning")
		e.field("thinking", value.Thinking)
		e.field("signature", value.Signature)
		e.field("thought_signature", value.ThoughtSignature)
		e.field("tool_id", value.ToolID)
		e.integer("started_at", value.StartedAt)
		e.integer("finished_at", value.FinishedAt)
		responses, err := json.Marshal(value.ResponsesData)
		if err != nil {
			return fmt.Errorf("marshal reasoning metadata: %w", err)
		}
		e.field("responses_data", string(responses))
	case message.TextContent:
		e.field("part_type", "text")
		e.field("text", value.Text)
	case message.ToolCall:
		e.field("part_type", "tool_call")
		e.field("id", value.ID)
		e.field("name", value.Name)
		e.field("input", value.Input)
		e.boolean("provider_executed", value.ProviderExecuted)
		e.boolean("finished", value.Finished)
	case message.ToolResult:
		e.field("part_type", "tool_result")
		encodeToolResult(e, value)
	case message.Finish:
		e.field("part_type", "finish")
		e.field("reason", string(value.Reason))
		e.integer("time", value.Time)
		e.field("message", value.Message)
		e.field("details", value.Details)
	default:
		return fmt.Errorf("unsupported content part %T", part)
	}
	return nil
}

func encodeToolResult(e *hashEncoder, result message.ToolResult) {
	e.field("tool_call_id", result.ToolCallID)
	e.field("name", result.Name)
	e.field("content", result.Content)
	e.field("data", result.Data)
	e.field("mime_type", result.MIMEType)
	e.field("metadata", result.Metadata)
	e.boolean("is_error", result.IsError)
}
