package agent

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/stretchr/testify/require"
)

func TestBuildAgentModelsUsesRequestedPrimaryModel(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	cfg.Config().Providers = csync.NewMapFrom(map[string]config.ProviderConfig{
		"mock": {
			ID:      "mock",
			Type:    openaicompat.Name,
			BaseURL: "http://127.0.0.1:9/v1",
			APIKey:  "test-key",
			Models: []catwalk.Model{
				{ID: "large-model", DefaultMaxTokens: 128},
				{ID: "small-model", DefaultMaxTokens: 128},
			},
		},
	})
	cfg.Config().Models = map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeLarge: {Provider: "mock", Model: "large-model"},
		config.SelectedModelTypeSmall: {Provider: "mock", Model: "small-model"},
	}

	coord := &coordinator{cfg: cfg}
	primary, small, err := coord.buildAgentModels(t.Context(), config.SelectedModelTypeSmall, true)
	require.NoError(t, err)
	require.Equal(t, "small-model", primary.ModelCfg.Model)
	require.Equal(t, "small-model", small.ModelCfg.Model)
}

func TestAgentToolUsesConfiguredSubagentModel(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Options.SubagentModel = config.SelectedModelTypeSmall
	cfg.SetupAgents()

	coord := &coordinator{cfg: cfg}
	unconfiguredAgent := config.Agent{}
	require.NoError(t, coord.applySubagentModel(&unconfiguredAgent, ""))
	require.Equal(t, config.SelectedModelTypeSmall, unconfiguredAgent.Model)

	cfg.Config().Options.SubagentModel = ""
	require.NoError(t, coord.applySubagentModel(&unconfiguredAgent, ""))
	require.Equal(t, config.SelectedModelTypeLarge, unconfiguredAgent.Model)
	cfg.Config().Options.SubagentModel = config.SelectedModelTypeSmall

	taskAgent, err := coord.subagentConfig("")
	require.NoError(t, err)
	require.Equal(t, config.SelectedModelTypeSmall, taskAgent.Model)
	require.NotContains(t, taskAgent.AllowedTools, "bash")

	generalPurposeAgent, err := coord.subagentConfig("general-purpose")
	require.NoError(t, err)
	require.Equal(t, config.SelectedModelTypeLarge, generalPurposeAgent.Model)
	require.Contains(t, generalPurposeAgent.AllowedTools, "bash")

	require.NoError(t, coord.applySubagentModel(&generalPurposeAgent, ""))
	require.Equal(t, config.SelectedModelTypeSmall, generalPurposeAgent.Model)

	require.NoError(t, coord.applySubagentModel(&generalPurposeAgent, config.SelectedModelTypeLarge))
	require.Equal(t, config.SelectedModelTypeLarge, generalPurposeAgent.Model)
}
