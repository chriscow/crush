package config_test

import (
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestShellConfigOptionBooleans(t *testing.T) {
	store := loadCrushSh(t, `option debug true
option progress false
option auto-lsp false`)

	opts := store.Config().Options
	require.True(t, opts.Debug, "debug should be on")
	require.NotNil(t, opts.Progress)
	require.False(t, *opts.Progress, "progress should be off")
	require.NotNil(t, opts.AutoLSP)
	require.False(t, *opts.AutoLSP, "auto-lsp should be off")
}

// Config phrases this field negatively (disable_metrics) but the command
// exposes it positively. "metrics false" must land as disable_metrics = true.
func TestShellConfigOptionPositiveMetricsFalse(t *testing.T) {
	store := loadCrushSh(t, `option metrics false`)
	require.True(t, store.Config().Options.DisableMetrics, "metrics off => disable_metrics true")
}

func TestShellConfigOptionSubagentModel(t *testing.T) {
	store := loadCrushSh(t, `option subagent-model small`)
	require.Equal(t, config.SelectedModelTypeSmall, store.Config().Options.SubagentModel)

	store.Config().SetupAgents()
	require.Equal(t, config.SelectedModelTypeSmall, store.Config().Agents[config.AgentTask].Model)
}

func TestShellConfigOptionRejectsInvalidSubagentModel(t *testing.T) {
	_, err := loadCrushShErr(t, `option subagent-model medium`)
	require.ErrorContains(t, err, "subagent model must be large or small")
}

// The bare positive form defaults to true, which inverts to disable = false.
func TestShellConfigOptionPositiveMetricsBare(t *testing.T) {
	store := loadCrushSh(t, `option metrics`)
	require.False(t, store.Config().Options.DisableMetrics, "metrics on => disable_metrics false")
}

func TestShellConfigOptionContextProjection(t *testing.T) {
	store := loadCrushSh(t, `provider add test --type openai-compat --base-url http://localhost:8080 --api-key test-key
model add test/test-model --name "Test Model"
model large test/test-model
option context-projection enabled true
option context-projection min-batch-chars 0
option context-projection keep-recent-batches 0
option context-projection summarizer-model large
option context-projection summarizer-timeout 90s`)

	opts := store.Config().Options.ContextProjection
	require.NotNil(t, opts)
	require.True(t, opts.Enabled)
	require.Equal(t, 0, opts.GetMinBatchChars())
	require.Equal(t, 0, opts.GetKeepRecentBatches())
	require.Equal(t, config.SelectedModelTypeLarge, opts.SummarizerModel)
	require.Equal(t, 90*time.Second, opts.SummarizerTimeout)
}

func TestShellConfigOptionContextProjectionRejectsInvalidValue(t *testing.T) {
	tests := []struct {
		name    string
		script  string
		wantErr string
	}{
		{name: "boolean", script: `option context-projection enabled maybe`, wantErr: "enabled expects true/false"},
		{name: "minimum batch characters", script: `option context-projection min-batch-chars -1`, wantErr: "min-batch-chars expects a non-negative integer"},
		{name: "recent batches", script: `option context-projection keep-recent-batches -1`, wantErr: "keep-recent-batches expects a non-negative integer"},
		{name: "model", script: `option context-projection summarizer-model medium`, wantErr: "summarizer-model expects large or small"},
		{name: "timeout", script: `option context-projection summarizer-timeout 0s`, wantErr: "summarizer-timeout expects a duration"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadCrushShErr(t, tt.script)
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestShellConfigOptionUI(t *testing.T) {
	store := loadCrushSh(t, `option ui compact true
option ui diff split
option ui transparent false
option ui mouse false
option ui scrollbar always
option ui completions-max-depth 4
option ui completions-max-items 200`)

	ui := store.Config().Options.TUI
	require.NotNil(t, ui)
	require.True(t, ui.CompactMode)
	require.Equal(t, "split", ui.DiffMode)
	require.NotNil(t, ui.Transparent)
	require.False(t, *ui.Transparent)
	require.NotNil(t, ui.Mouse)
	require.False(t, *ui.Mouse)
	require.Equal(t, "always", ui.Scrollbar)
	require.NotNil(t, ui.Completions.MaxDepth)
	require.Equal(t, 4, *ui.Completions.MaxDepth)
	require.NotNil(t, ui.Completions.MaxItems)
	require.Equal(t, 200, *ui.Completions.MaxItems)
}

func TestShellConfigOptionUIRejectsInvalidValue(t *testing.T) {
	_, err := loadCrushShErr(t, `option ui diff side-by-side`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "expects unified or split")
}

func TestShellConfigOptionAttribution(t *testing.T) {
	store := loadCrushSh(t, `option attribution-trailer-style none
option attribution-generated-with false`)

	attribution := store.Config().Options.Attribution
	require.NotNil(t, attribution)
	require.Equal(t, "none", string(attribution.TrailerStyle))
	require.False(t, attribution.GeneratedWith)
}

func TestShellConfigOptionAttributionTrailerStylePreservesGeneratedWithDefault(t *testing.T) {
	store := loadCrushSh(t, `option attribution-trailer-style co-authored-by`)

	attribution := store.Config().Options.Attribution
	require.NotNil(t, attribution)
	require.Equal(t, "co-authored-by", string(attribution.TrailerStyle))
	require.True(t, attribution.GeneratedWith)
}

func TestShellConfigOptionAttributionGeneratedWithCaseInsensitive(t *testing.T) {
	store := loadCrushSh(t, `option attribution-generated-with YES`)

	require.NotNil(t, store.Config().Options.Attribution)
	require.True(t, store.Config().Options.Attribution.GeneratedWith)
}

func TestShellConfigOptionAttributionRejectsInvalidStyle(t *testing.T) {
	_, err := loadCrushShErr(t, `option attribution-trailer-style bogus`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "expects none, co-authored-by, or assisted-by")
}

func TestShellConfigOptionListAppends(t *testing.T) {
	store := loadCrushSh(t, `option disable-skill crush-config
option disable-skill jq`)

	require.Subset(t, store.Config().Options.DisabledSkills, []string{"crush-config", "jq"})
}

// reset wipes values added earlier (or via source) while keeping anything
// added after it — observable in the effective config.
func TestShellConfigOptionReset(t *testing.T) {
	store := loadCrushSh(t, `option skill-path ./inherited-a
option skill-path ./inherited-b
option reset skill-path
option skill-path ./mine`)

	paths := store.Config().Options.SkillsPaths
	require.Contains(t, paths, "./mine")
	require.NotContains(t, paths, "./inherited-a")
	require.NotContains(t, paths, "./inherited-b")
}

func TestShellConfigOptionUnknownKeyFails(t *testing.T) {
	_, err := loadCrushShErr(t, `option bogus-key value`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown key")
}

func TestShellConfigOptionDisableToolRemoved(t *testing.T) {
	_, err := loadCrushShErr(t, `option disable-tool bash`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown key")
}

func TestShellConfigOptionResetRejectsNonList(t *testing.T) {
	_, err := loadCrushShErr(t, `option reset debug`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not one")
}
