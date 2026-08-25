package agent

import (
	"context"
	_ "embed"
	"errors"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
)

//go:embed templates/agent_tool.md
var agentToolDescription string

type AgentParams struct {
	Prompt       string                   `json:"prompt" description:"The task for the agent to perform"`
	SubagentType string                   `json:"subagent_type,omitempty" description:"Type of sub-agent to spawn, e.g. general-purpose"`
	Model        config.SelectedModelType `json:"model,omitempty" enum:"large,small" description:"Model type for the sub-agent. Defaults to the configured subagent model, which is large by default"`
}

const (
	AgentToolName = "agent"
)

func (c *coordinator) agentTool(_ctx context.Context) (fantasy.AgentTool, error) {
	return fantasy.NewParallelAgentTool(
		AgentToolName,
		agentToolDescription,
		func(ctx context.Context, params AgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
			}
			if params.Model != "" && params.Model != config.SelectedModelTypeLarge && params.Model != config.SelectedModelTypeSmall {
				return fantasy.NewTextErrorResponse("model must be large or small"), nil
			}

			sessionID := tools.GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}

			agentMessageID := tools.GetMessageFromContext(ctx)
			if agentMessageID == "" {
				return fantasy.ToolResponse{}, errors.New("agent message id missing from context")
			}

			agentCfg, err := c.subagentConfig(params.SubagentType)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if err := c.applySubagentModel(&agentCfg, params.Model); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			promptObj, err := taskPrompt(prompt.WithWorkingDir(c.cfg.WorkingDir()))
			if err != nil {
				return fantasy.ToolResponse{}, err
			}

			agent, err := c.buildAgent(ctx, promptObj, agentCfg, true)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}

			return c.runSubAgent(ctx, subAgentParams{
				Agent:          agent,
				SessionID:      sessionID,
				AgentMessageID: agentMessageID,
				ToolCallID:     call.ID,
				Prompt:         params.Prompt,
				SessionTitle:   "New Agent Session",
			})
		},
	), nil
}

func (c *coordinator) subagentConfig(subagentType string) (config.Agent, error) {
	agentID := config.AgentTask
	if subagentType == "general-purpose" {
		agentID = config.AgentCoder
	}
	agentCfg, ok := c.cfg.Config().Agents[agentID]
	if !ok {
		agentCfg, ok = c.cfg.Config().Agents[config.AgentTask]
		if !ok {
			return config.Agent{}, errors.New("task agent not configured")
		}
	}
	return agentCfg, nil
}

func (c *coordinator) applySubagentModel(agentCfg *config.Agent, model config.SelectedModelType) error {
	if model == "" {
		model = c.cfg.Config().Options.SubagentModel
	}
	if model == "" {
		model = config.SelectedModelTypeLarge
	}
	if model != config.SelectedModelTypeLarge && model != config.SelectedModelTypeSmall {
		return errors.New("model must be large or small")
	}
	agentCfg.Model = model
	return nil
}
