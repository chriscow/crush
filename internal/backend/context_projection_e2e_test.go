package backend

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/agent"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/condense"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/question"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

const (
	projectionMainProvider    = "projection-main"
	projectionSummaryProvider = "projection-summary"
	projectionMainModel       = "main-model"
	projectionSummaryModel    = "summary-model"
)

type projectionRequest struct {
	Model  string          `json:"model"`
	Stream bool            `json:"stream"`
	Body   json.RawMessage `json:"-"`
	Fields map[string]any  `json:"-"`
}

type projectionProvider struct {
	mu       sync.Mutex
	requests []projectionRequest
	server   *httptest.Server
}

func newProjectionProvider(t *testing.T) *projectionProvider {
	t.Helper()
	provider := &projectionProvider{}
	provider.server = httptest.NewServer(http.HandlerFunc(provider.serveHTTP))
	t.Cleanup(provider.server.Close)
	return provider
}

func (p *projectionProvider) serveHTTP(w http.ResponseWriter, request *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	model, _ := body["model"].(string)
	stream, _ := body["stream"].(bool)
	p.mu.Lock()
	p.requests = append(p.requests, projectionRequest{Model: model, Stream: stream, Body: encoded, Fields: body})
	p.mu.Unlock()

	if model == projectionSummaryModel {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"summary","object":"chat.completion","created":1,"model":"summary-model","choices":[{"index":0,"message":{"role":"assistant","content":"{\"summary\":\"fixture batch summarized\",\"items\":[{\"ordinal\":0,\"description\":\"fixture output\"}]}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
		return
	}
	if model != projectionMainModel {
		http.Error(w, "unknown model", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	_, _ = fmt.Fprint(w, "data: {\"id\":\"main\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"main-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":null}]}\n\n")
	_, _ = fmt.Fprint(w, "data: {\"id\":\"main\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"main-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	_, _ = fmt.Fprint(w, "data: {\"id\":\"main\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"main-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":1,\"total_tokens\":21}}\n\n")
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

func (p *projectionProvider) requestsFor(model string) []projectionRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	var requests []projectionRequest
	for _, request := range p.requests {
		if request.Model == model {
			requests = append(requests, request)
		}
	}
	return requests
}

func projectionRequestText(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case []any:
		var text strings.Builder
		for _, item := range value {
			text.WriteString(projectionRequestText(item))
		}
		return text.String()
	case map[string]any:
		var text strings.Builder
		for _, item := range value {
			text.WriteString(projectionRequestText(item))
		}
		return text.String()
	default:
		return ""
	}
}

type projectionRuntime struct {
	conn        *sql.DB
	queries     *db.Queries
	store       *condense.Store
	sessions    session.Service
	messages    message.Service
	coordinator agent.Coordinator
	cancel      context.CancelFunc
}

func newProjectionRuntime(t *testing.T, cfg *config.ConfigStore, dataDir, workDir string) *projectionRuntime {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	queries := db.New(conn)
	store := condense.NewStore(queries, conn)
	sessions := session.NewService(queries, conn, session.WithDeleteLifecycle(store))
	messages := message.NewService(queries, message.WithMessageDeleter(store), message.WithDebounce(0))
	coordinator, err := agent.NewCoordinator(ctx, agent.CoordinatorOptions{
		Config: cfg, Sessions: sessions, Messages: messages,
		Permissions: permission.NewPermissionService(workDir, true, nil),
		Questions:   question.NewService(), History: history.NewService(queries, conn),
		FileTracker: filetracker.NewService(queries), LSPManager: lsp.NewManager(cfg),
		Interactive: true, ProjectionStore: store,
	})
	require.NoError(t, err)
	return &projectionRuntime{
		conn: conn, queries: queries, store: store, sessions: sessions,
		messages: messages, coordinator: coordinator, cancel: cancel,
	}
}

func projectionTestConfig(t *testing.T, workDir, dataDir, baseURL string) *config.ConfigStore {
	t.Helper()
	cfg, err := config.Init(workDir, dataDir, false)
	require.NoError(t, err)
	cfg.Config().Providers = csync.NewMapFrom(map[string]config.ProviderConfig{
		projectionMainProvider: {
			ID: projectionMainProvider, Type: openaicompat.Name, BaseURL: baseURL + "/v1", APIKey: "test-key",
			Models: []catwalk.Model{{ID: projectionMainModel, Name: "Main", ContextWindow: 200_000, DefaultMaxTokens: 1_024}},
		},
		projectionSummaryProvider: {
			ID: projectionSummaryProvider, Type: openaicompat.Name, BaseURL: baseURL + "/v1", APIKey: "test-key",
			Models: []catwalk.Model{{ID: projectionSummaryModel, Name: "Summary", ContextWindow: 200_000, DefaultMaxTokens: 1_024}},
		},
	})
	cfg.Config().Models = map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeLarge: {Provider: projectionMainProvider, Model: projectionMainModel},
		config.SelectedModelTypeSmall: {Provider: projectionSummaryProvider, Model: projectionSummaryModel},
	}
	zero := 0
	autoLSP := false
	cfg.Config().Options.DataDirectory = dataDir
	cfg.Config().Options.DisableAutoSummarize = true
	cfg.Config().Options.AutoLSP = &autoLSP
	cfg.Config().Options.ContextProjection = &config.ContextProjectionOptions{
		Enabled: true, MinBatchChars: &zero, KeepRecentBatches: &zero,
		SummarizerModel: config.SelectedModelTypeSmall, SummarizerTimeout: 5 * time.Second,
	}
	cfg.SetupAgents()
	coder := cfg.Config().Agents[config.AgentCoder]
	coder.AllowedTools = nil
	cfg.Config().Agents[config.AgentCoder] = coder
	return cfg
}

func recoverProjection(t *testing.T, store *condense.Store, sessionID, ref string) string {
	t.Helper()
	tool := agenttools.NewContextTreeQueryTool(condense.New(store, nil, condense.Options{}))
	ctx := context.WithValue(t.Context(), agenttools.SessionIDContextKey, sessionID)
	limit := 137
	offset := 0
	var recovered strings.Builder
	for {
		input, err := json.Marshal(agenttools.ContextTreeQueryParams{Ref: ref, Offset: offset, Limit: &limit})
		require.NoError(t, err)
		response, err := tool.Run(ctx, fantasy.ToolCall{ID: fmt.Sprintf("recover-%d", offset), Name: agenttools.ContextTreeQueryToolName, Input: string(input)})
		require.NoError(t, err)
		require.False(t, response.IsError)
		var page agenttools.ContextTreeQueryResult
		require.NoError(t, json.Unmarshal([]byte(response.Content), &page))
		require.Equal(t, "ok", page.Status)
		recovered.WriteString(page.Content)
		if page.NextOffset == nil {
			require.True(t, page.Complete)
			break
		}
		offset = *page.NextOffset
	}
	return recovered.String()
}

func TestContextProjectionEndToEnd(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	provider := newProjectionProvider(t)
	workDir := t.TempDir()
	dataDir := t.TempDir()
	cfg := projectionTestConfig(t, workDir, dataDir, provider.server.URL)
	runtime := newProjectionRuntime(t, cfg, dataDir, workDir)

	sess, err := runtime.sessions.Create(t.Context(), "Projection E2E")
	require.NoError(t, err)
	_, err = runtime.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "seed"}},
	})
	require.NoError(t, err)
	_, err = runtime.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: "call-1", Name: "view", Input: `{"path":"fixture.txt"}`, Finished: true},
			message.Finish{Reason: message.FinishReasonToolUse},
		},
	})
	require.NoError(t, err)
	originalBody := strings.Repeat("canonical-🙂-payload\n", 500)
	toolMessage, err := runtime.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.Tool,
		Parts: []message.ContentPart{message.ToolResult{ToolCallID: "call-1", Name: "view", Content: originalBody}},
	})
	require.NoError(t, err)

	result, err := runtime.coordinator.Run(t.Context(), sess.ID, "continue")
	require.NoError(t, err)
	require.Equal(t, "done", result.Response.Content.Text())
	summaryRequests := provider.requestsFor(projectionSummaryModel)
	mainRequests := provider.requestsFor(projectionMainModel)
	require.Len(t, summaryRequests, 1)
	require.Len(t, mainRequests, 1)
	require.False(t, summaryRequests[0].Stream)
	mainBody := string(mainRequests[0].Body)
	require.Contains(t, mainBody, "[Context projection]")
	require.Contains(t, mainBody, "fixture batch summarized")
	require.Contains(t, mainBody, "t1")
	require.Contains(t, mainBody, "context_tree_query")
	require.Contains(t, mainBody, "call-1")
	require.NotContains(t, mainBody, originalBody)

	canonical, err := runtime.queries.GetMessage(t.Context(), toolMessage.ID)
	require.NoError(t, err)
	parts, err := message.DecodeParts([]byte(canonical.Parts))
	require.NoError(t, err)
	require.Equal(t, originalBody, parts[0].(message.ToolResult).Content)
	require.Equal(t, originalBody, recoverProjection(t, runtime.store, sess.ID, "t1"))

	runtime.coordinator.CancelAll()
	runtime.cancel()
	require.NoError(t, db.Release(dataDir))
	runtime = newProjectionRuntime(t, cfg, dataDir, workDir)
	require.Equal(t, originalBody, recoverProjection(t, runtime.store, sess.ID, "t1"))

	backend, _ := newTestBackend(t)
	workspace := &Workspace{
		App: &app.App{Sessions: runtime.sessions, Messages: runtime.messages},
		ID:  "workspace", Path: workDir, clients: make(map[string]*clientState),
	}
	backend.workspaces.Set(workspace.ID, workspace)
	forked, err := backend.ForkSession(t.Context(), workspace.ID, sess.ID)
	require.NoError(t, err)
	forkNodes, err := runtime.queries.CountContextProjectionNodesBySession(t.Context(), forked.ID)
	require.NoError(t, err)
	require.Zero(t, forkNodes)
	_, err = runtime.coordinator.Run(t.Context(), forked.ID, "continue fork")
	require.NoError(t, err)
	forkNodes, err = runtime.queries.CountContextProjectionNodesBySession(t.Context(), forked.ID)
	require.NoError(t, err)
	require.Positive(t, forkNodes)
	forkActive, err := runtime.queries.ListActiveContextProjectionNodes(t.Context(), db.ListActiveContextProjectionNodesParams{
		SessionID: forked.ID, AlgorithmVersion: condense.AlgorithmVersion,
	})
	require.NoError(t, err)
	require.Len(t, forkActive, 1)
	forkItems, err := runtime.queries.ListContextProjectionItemsByNode(t.Context(), forkActive[0].ID)
	require.NoError(t, err)
	require.Greater(t, forkItems[0].RefNumber, int64(1))

	cfg.Config().Options.ContextProjection.Enabled = false
	_, err = runtime.coordinator.Run(t.Context(), sess.ID, "after disable")
	require.NoError(t, err)
	mainRequests = provider.requestsFor(projectionMainModel)
	require.Len(t, mainRequests, 3)
	disabledBody := string(mainRequests[2].Body)
	disabledMessages := projectionRequestText(mainRequests[2].Fields["messages"])
	require.Contains(t, disabledMessages, originalBody)
	require.NotContains(t, disabledMessages, "[Context projection]")
	require.Contains(t, disabledBody, "context_tree_query")
	require.Len(t, provider.requestsFor(projectionSummaryModel), 2)
	require.Equal(t, originalBody, recoverProjection(t, runtime.store, sess.ID, "t1"))

	runtime.coordinator.CancelAll()
	runtime.cancel()
	require.NoError(t, db.Release(dataDir))
	db.ResetPool()
}
