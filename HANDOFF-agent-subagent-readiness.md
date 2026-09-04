# Handoff: Fix delegated-agent readiness and validation

## Objective
Fix the delegated-agent implementation problems identified in the recent Crush changes, especially the readiness race introduced by creating delegated agents at tool invocation time.

## Scope
Work in `/Users/chris/dev/crush` on the `feat/context-projection` branch. Do not disturb the unrelated, uncommitted context-projection work already present in the tree.

## Confirmed findings

### 1. Delegated agents can run before their prompt and tools are initialized
`agentTool` now builds an agent for each invocation and immediately sends it to `runSubAgent`.

- Agent construction: `internal/agent/agent_tool.go`
- `buildAgent` launches asynchronous prompt/tool initialization using the coordinator-global `readyWg`: `internal/agent/coordinator.go`
- `runSubAgent` directly invokes `params.Agent.Run`, so it does not pass through `coordinator.run`, which is the only path that waits for `readyWg`.
- `sessionAgent.Run` snapshots its system prompt and tools at entry: `internal/agent/agent.go`.

A dynamic delegated agent can therefore run with an empty system prompt and empty tool palette.

### 2. A delegated-agent initialization failure can poison later top-level turns
The same dynamic initialization jobs contribute errors to the coordinator-global `readyWg`. The top-level `coordinator.run` waits on that group at the start of every turn. An initialization error from any ephemeral delegated agent can thus make later ordinary turns fail.

The coordinator readiness group should only express coordinator startup readiness; each ephemeral agent needs isolated readiness handling.

### 3. `subagent_type` accepts typos silently
`subagentConfig` uses the coder configuration only for the exact value `general-purpose`; every other string selects the read-only task configuration. The parameter is not an enum in the generated tool schema.

Validate the accepted type values and return a tool error for unsupported input.

### 4. Decide whether recursive general-purpose delegation is desired
A `general-purpose` delegated agent inherits the coder agent’s allowed tool list, including `agent`. `isSubAgent` does not remove that tool. This newly permits unbounded nested delegation. Decide whether to disallow nested `agent` calls or add an explicit depth/budget policy; do not silently change product behavior without checking project conventions or asking the user.

## Relevant history
- `678d197f feat(agent): allow subagent_type to select general-purpose agent`
- `7bb40534 feat: allow selecting models for delegated agents`
- `5205e8ac fix: serialize skill reloads and scheduled runs`

Inspect these commits directly for the precise diff. The prior implementation of `agentTool` eagerly built one task agent during coordinator tool construction, before top-level readiness was awaited.

## Existing tests and validation
Targeted existing tests passed:

```sh
cd /Users/chris/dev/crush
go test ./internal/agent -run '^(TestBuildAgentModelsUsesRequestedPrimaryModel|TestAgentToolUsesConfiguredSubagentModel|TestAgentToolSchemaIncludesModel|TestAgentToolRejectsInvalidModel)$' -count=1
```

A full `go test ./internal/agent` was attempted and failed in existing recorded-provider interaction fixtures whose expected request payloads no longer matched dynamic prompt/skill content. Do not characterize that as a failure of this change without independently reproducing it on a clean baseline.

Add deterministic regression tests for:
- a newly created delegated agent not entering `Run` before prompt and tools are ready;
- an ephemeral-agent initialization failure not making a later top-level coordinator run fail;
- invalid `subagent_type` rejection.

## Implementation direction
Prefer a per-agent readiness object/group returned or owned by the dynamically constructed agent. Ensure `runSubAgent` waits for *that agent only* before invoking `Run`; errors should be returned to that tool invocation rather than written into global coordinator readiness state.

Do not solve this by waiting on the shared `c.readyWg` in `runSubAgent`: it mixes unrelated delegates, retains errors globally, and cannot provide proper lifecycle ownership.

## Working-tree warning
The working tree contains unrelated modifications and untracked context-projection files. Preserve them. Review `git status --short` before editing and stage only files belonging to this fix.

## Suggested skills
- `diagnosing-bugs` — follow the evidence-first bug-fix loop and write regression tests.
- `go-idioms` — concurrency/lifecycle API and error-handling review.
- `go-testing` — deterministic tests for goroutine readiness and error isolation.
- `tdd` — if implementing the regression tests before changing production code.
- `ask-user` — before imposing a recursive-delegation policy, if project documentation does not settle it.
