package shellconfig

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOption_Bool(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option debug true
option progress false`
	path := filepath.Join(dir, "crushrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, true, opts["debug"])
	require.Equal(t, false, opts["progress"])
}

func TestOption_BoolCaseInsensitive(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option debug TRUE
option progress False
option metrics YES`
	path := filepath.Join(dir, "crushrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, true, opts["debug"])
	require.Equal(t, false, opts["progress"])
	require.Equal(t, false, opts["disable_metrics"])
}

func TestOption_String(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option data-directory .crush
option notifications osc
option subagent-model small`
	path := filepath.Join(dir, "crushrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, ".crush", opts["data_directory"])
	require.Equal(t, "osc", opts["notifications"])
	require.Equal(t, "small", opts["subagent_model"])
}

func TestOption_List(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option context-path .cursorrules
option context-path CRUSH.md`
	path := filepath.Join(dir, "crushrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	paths := opts["context_paths"].([]any)
	require.Len(t, paths, 2)
	require.Equal(t, ".cursorrules", paths[0])
	require.Equal(t, "CRUSH.md", paths[1])
}

func TestOption_Reset(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option skill-path ./a
option skill-path ./b
option reset skill-path`
	path := filepath.Join(dir, "crushrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Empty(t, opts["skills_paths"].([]any))
}

func TestOption_ResetThenReadd(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option skill-path ./inherited-a
option skill-path ./inherited-b
option reset skill-path
option skill-path ./mine`
	path := filepath.Join(dir, "crushrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	paths := opts["skills_paths"].([]any)
	require.Len(t, paths, 1)
	require.Equal(t, "./mine", paths[0])
}

func TestOption_ResetUnknownKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option reset bogus-key`
	path := filepath.Join(dir, "crushrc")

	_, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown key")
}

func TestOption_ResetNonListKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option reset debug`
	path := filepath.Join(dir, "crushrc")

	_, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.Error(t, err)
	require.Contains(t, err.Error(), "not one")
}

func TestOption_UIUnknownKey(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "crushrc")
	_, err := LoadShellConfig(t.Context(), path, []byte(`option ui bogus true`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown key")
}

func TestOption_UIExitBanner(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "crushrc")
	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(`option ui exit-banner compact`))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	ui := result["options"].(map[string]any)["tui"].(map[string]any)
	require.Equal(t, "compact", ui["exit_banner"])

	_, err = LoadShellConfig(t.Context(), path, []byte(`option ui exit-banner bogus`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "expects default, compact, or none")
}

func TestOption_BoolShorthand(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option debug
option metrics`
	path := filepath.Join(dir, "crushrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, true, opts["debug"])
	require.Equal(t, false, opts["disable_metrics"])
}

func TestOption_InvertedBool(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option metrics false`
	path := filepath.Join(dir, "crushrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, true, opts["disable_metrics"])
}

func TestOption_ContextProjection(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "crushrc")
	script := `option context-projection enabled true
option context-projection min-batch-chars 0
option context-projection keep-recent-batches 0
option context-projection summarizer-model large
option context-projection summarizer-timeout 90s`

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	projection := opts["context_projection"].(map[string]any)
	require.Equal(t, true, projection["enabled"])
	require.Equal(t, float64(0), projection["min_batch_chars"])
	require.Equal(t, float64(0), projection["keep_recent_batches"])
	require.Equal(t, "large", projection["summarizer_model"])
	require.Equal(t, float64(90*time.Second), projection["summarizer_timeout"])
}

func TestOption_ContextProjectionRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		script  string
		wantErr string
	}{
		{name: "boolean", script: `option context-projection enabled sometimes`, wantErr: "enabled expects true/false"},
		{name: "minimum batch characters", script: `option context-projection min-batch-chars -1`, wantErr: "min-batch-chars expects a non-negative integer"},
		{name: "recent batches", script: `option context-projection keep-recent-batches nope`, wantErr: "keep-recent-batches expects a non-negative integer"},
		{name: "model", script: `option context-projection summarizer-model medium`, wantErr: "summarizer-model expects large or small"},
		{name: "timeout syntax", script: `option context-projection summarizer-timeout soon`, wantErr: "summarizer-timeout expects a duration"},
		{name: "timeout zero", script: `option context-projection summarizer-timeout 0s`, wantErr: "summarizer-timeout expects a duration"},
		{name: "timeout bound", script: `option context-projection summarizer-timeout 2h`, wantErr: "summarizer-timeout expects a duration"},
		{name: "unknown key", script: `option context-projection bogus value`, wantErr: "unknown key"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "crushrc")
			_, err := LoadShellConfig(t.Context(), path, []byte(tt.script))
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestOption_UnknownKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option bogus-key value`
	path := filepath.Join(dir, "crushrc")

	_, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown key")
}

func TestOption_RequestTimeout(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option request-timeout 300
option request-timeout 0`
	path := filepath.Join(dir, "crushrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, float64(0), opts["request_timeout"])
}

func TestOption_RequestTimeoutInvalid(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option request-timeout soon`
	path := filepath.Join(dir, "crushrc")

	_, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.Error(t, err)
	require.Contains(t, err.Error(), "expects a number of seconds")
}
