package agent

import (
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestAgentToolSchemaIncludesModel(t *testing.T) {
	env := testEnv(t)
	coord := newTestCoordinator(t, env, "mock", config.ProviderConfig{})

	tool, err := coord.agentTool(t.Context())
	require.NoError(t, err)

	parameters := tool.Info().Parameters
	model, ok := parameters["model"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, []any{"large", "small"}, model["enum"])
}

func TestAgentToolRejectsInvalidModel(t *testing.T) {
	env := testEnv(t)
	coord := newTestCoordinator(t, env, "mock", config.ProviderConfig{})

	tool, err := coord.agentTool(t.Context())
	require.NoError(t, err)

	response, err := tool.Run(t.Context(), fantasy.ToolCall{
		Input: `{"prompt":"inspect the repository","model":"medium"}`,
	})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Equal(t, "model must be large or small", response.Content)
}
