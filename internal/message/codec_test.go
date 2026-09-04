package message

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodePartsRoundTrip(t *testing.T) {
	t.Parallel()

	want := []ContentPart{
		ReasoningContent{
			Thinking:         "think",
			Signature:        "signature",
			ThoughtSignature: "thought",
			ToolID:           "tool-id",
			StartedAt:        1,
			FinishedAt:       2,
		},
		TextContent{Text: "text"},
		ImageURLContent{URL: "https://example.test/image.png", Detail: "high"},
		BinaryContent{Path: "file.bin", MIMEType: "application/octet-stream", Data: []byte{0, 1, 2}},
		ToolCall{ID: "call", Name: "view", Input: `{"path":"x"}`, ProviderExecuted: true, Finished: true},
		ToolResult{ToolCallID: "call", Name: "view", Content: "result", Data: "", MIMEType: "", Metadata: `{"lines":1}`, IsError: true},
		Finish{Reason: FinishReasonToolUse, Time: 3, Message: "message", Details: "details"},
		ShellCommand{Command: "pwd", Output: "/tmp", ExitCode: 0},
	}

	encoded, err := marshalParts(want)
	require.NoError(t, err)

	got, err := DecodeParts(encoded)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestDecodePartsRejectsInvalidData(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
	}{
		{name: "invalid json", data: "{"},
		{name: "invalid wrapper", data: `[{"type":`},
		{name: "unknown type", data: `[{"type":"future","data":{}}]`},
		{name: "invalid part", data: `[{"type":"tool_result","data":"bad"}]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := DecodeParts([]byte(tt.data))
			require.Error(t, err)
		})
	}
}
