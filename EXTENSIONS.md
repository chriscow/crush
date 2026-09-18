# Crush Extension System — Design and Independent Audit

Status: proposal. This document specifies required behavior, not an implemented
extension interface. No extension framework implementation is included.
The Go blocks are focused contract sketches. The surrounding sections specify
larger interfaces behaviorally; they are not a complete compiled registry API.

## 1. Audit scope and conclusion

The audit compares local `upstream/main` (`abc246403b95`) with branch
`feat/context-projection` at `904a0afb6f52`, on 2026-09-18. The comparison contains
20 commits and 153 changed files: 17,816 added and 483 deleted text lines, plus
13 changed binary cassette files. These are local refs, not a claim about the
latest upstream branch or the current status of upstream pull requests.

The checkout also contains uncommitted edits to
`internal/ui/model/sidebar.go` and `sidebar_projection_test.go`. They are
outside the committed comparison and must remain intact. `EXTENSIONS.md` was
untracked before this audit.

In-process Go extensions can isolate substantial feature logic. The original
proposal was not ready to implement. Its strongest ideas were typed config,
feature-owned SQL, provider construction hooks, and shared client/server
commands. Its principal gaps were:

- The proposed process-wide instance has no workspace isolation contract.
- Import order does not define Go package initialization or middleware order.
- Core query objects cannot execute extension-owned generated SQL in a shared
  transaction. Separate usage updates also lose projection's atomic accounting.
- An enqueue-only prompt call loses the scheduler's actual execution outcome.
- Config validation needs tool declarations before runtime tool construction.
- Server routes alone do not initialize client UI, decode events, or cache state.
- Disabling all hooks after a panic or config change can break stored recovery
  references and database cleanup.
- The proposed host line estimates omit required implementations and tests.

The recommended approach is incremental extraction behind narrow interfaces.
Keep core data-integrity behavior in core. Add shared extension machinery only
where more than one feature benefits. A feature that needs a new kind of host
behavior will still require a core edit. Moving code into another package does
not, by itself, remove coupling to upstream behavior.

## 2. Terms, goals, and limits

A **session** is a stored conversation. A **turn** is one accepted user prompt
and its execution. A **run ID** distinguishes that execution from other turns
on the same session. A **workspace** owns an application runtime and config.
Several workspaces can exist in one server process.

An **extension** is trusted Go code compiled into the binary. A **descriptor**
is immutable process-wide metadata. An **instance** holds mutable state for one
workspace. A **capability** is one supported contribution, such as a tool or
storage callback. A **seam** is the core integration point for that capability.

Goals:

- Keep feature logic and its tests together, behind small interfaces.
- Reuse the same backend behavior in local and client/server modes.
- Preserve existing persistence, cancellation, permission, and recovery rules.
- Reduce repeated edits during upstream merges, without promising zero edits.

Non-goals:

- Loading uncompiled packages, a public stable plugin ABI, or sandboxing.
- Replacing every core feature with a generic extension.
- Automatically generating arbitrary dialogs, CLI syntax, or provider behavior.
- Treating all of the fork diff as removable production host code.

The composition file must import each feature and list its descriptor/factory.
Prefer an explicit list over blank imports and mutable `init()` registration.
This costs an import and a list entry per extension. It makes test registries,
ordering, and workspace ownership explicit.

## 3. Package ownership and activation

### 3.1 Dependencies

Use `internal/ext` for backend contracts and registry composition. Keep UI
contracts separate, for example in `internal/extui`. The composition root may
import feature implementations. The contract packages must not import those
implementations.

Do not make `ext` import `message`, `agent`, `config`, or `workspace` while those
packages also import `ext`. Go rejects that cycle. Two workable arrangements
are consumer-owned callback interfaces injected by `app`, or shared leaf data
types with adapters in `app`. Prefer existing consumer-owned interfaces where
they suffice. A storage callback can use a structural `DBTX` interface without
making the database migration package import the runtime registry.

The registry is not a replacement for the large frontend `Workspace`
interface. Add a small extension client interface beside it and implement local
and remote adapters. Avoid adding a feature-specific method to every workspace
and coordinator fake.

### 3.2 Static declarations and runtime claims

Each descriptor declares:

| Declaration | Required behavior |
|---|---|
| Identity | Stable lowercase ID, config version, wire version, and dependencies. Reject duplicate IDs and dependency cycles. |
| Config | Factory for fresh default options, supported fields, decoder, validation, and schema. No shared mutable options pointer. |
| Tool manifest | Names, agent eligibility, replacements, and reserved recovery names. Available during config validation. |
| Migrations | Embedded SQL rooted at a defined directory, with a separate extension version ledger. |
| Storage/recovery factory | Creates required data adapters independently of runtime enablement, after migrations. |
| Runtime factory | Creates a fresh backend instance per workspace. |
| Presentation factory | Creates a fresh UI adapter per client/workspace without opening a database or starting backend workers. |
| Stats factory | Creates a collector that accepts a read-only database handle without starting agents or workers. |

Order descriptors by dependencies, then by stable ID for ties. Preserve local
claim order within an instance. Freeze a registry generation before use.
Request middleware uses that order on entry and reverse order on exit.
Tool presentation remains sorted by name, as `buildTools` currently does.

Import-list order is not a valid ordering mechanism. The Go specification
orders eligible package initialization by import path, after dependencies.
See [Go program initialization](https://go.dev/ref/spec#Program_initialization_and_execution).

The storage/recovery factory receives storage and logging dependencies, not a
running agent. It registers transaction callbacks and a recovery-tool factory
before enabled runtime claims are prepared. It must not start network work or
background services. The host owns its cleanup until after all operations stop.
Factory failure aborts workspace activation. Recovery tools are constructed in
the Build phase even when the feature's new activity is disabled. Accounting
reconciliation for disabled features is an explicit storage operation, not an
untracked worker.

Runtime claims are staged privately. Validate all names, dependencies, policies,
and factories before publishing the generation. An `Init` error must not leave
half-registered tools or callbacks. Initialization may allocate resources, but
must return cleanup for those resources and must not start untracked workers.

### 3.3 Startup phases

| Phase | Work allowed |
|---|---|
| Composition | Collect immutable descriptors. No database access, config resolution, or workers. |
| Config | Load core and extension config together. Validate fields and declared tool names. Resolve enablement. |
| Database | Run core migrations, then compiled extensions' migrations in descriptor order, once per physical pooled database lifecycle, under database-open serialization. |
| Storage | Install required integrity/recovery adapters for stored feature data, including disabled features. |
| Prepare | Create workspace instances and stage their capabilities. Config, paths, storage, and logging are available. |
| Build | Freeze capabilities. Build provider/model factories, agents, tools, and prompt contributions. |
| Ready | Publish the workspace runtime. Commands and prompt dispatch become available. |
| Workers | Start services with the ready workspace context. |

Model construction during Prepare must either be deferred or use the fully
staged provider registry. An extension cannot call a half-built model factory.
Agent execution, agent reload, and commands return `not_ready` before Ready.
An unconfigured workspace can expose config and read-only state, but cannot run
agents until its provider/model setup succeeds.

The backend owns migrations, tools, storage, and workers in client/server mode.
The client initializes only presentation adapters from the server's advertised
capabilities and snapshots. Local mode uses the same presentation contract with
an in-process backend adapter.

Current integration points are `Backend.CreateWorkspace` in
`internal/backend/backend.go`, `app.New`, the local initialization paths under
`internal/cmd`, and `db.Connect`. Changing only `app.New` misses config loading,
CLI session operations, read-only stats, and remote UI construction.

### 3.4 Disablement, failure, and shutdown

Enablement controls new feature activity. It does not remove persisted data or
required storage/recovery adapters. In the first version, adding/removing an
instance requires workspace restart. Live option changes use the validated
reload path below. Disabling projection must leave visible references readable
and cleanup hooks active. A restarted workspace with cron disabled starts no cron
workers. A live enablement edit reports `restart_required` and leaves the current
instance unchanged. Workspace shutdown cancels and settles its in-flight work.

Duplicate capabilities, migration failures, missing integrity adapters, and
failed preparation abort workspace activation. Do not silently start a workspace
with incomplete persistence behavior. An explicitly optional presentation
adapter may fail with a visible unavailable state.

On shutdown, reject new commands and dispatch, cancel workers and accepted runs,
wait for completion/accounting, flush buffered messages, then close adapters and
subscriptions in reverse dependency order. Release the database last. Use bounded
cleanup contexts that survive turn cancellation. If a worker does not stop before
the deadline, report shutdown failure. Go cannot forcibly terminate a goroutine
or make closing its database safe. `app.Shutdown` currently runs cleanup functions
in parallel, so extension cleanup cannot simply be appended to that list.

## 4. Configuration and reload

`ConfigSpec` must describe supported data, not promise arbitrary Go reflection.
The initial subset is exported JSON-tagged structs, booleans, integers, strings,
string lists, enums, and explicitly encoded durations. Each load creates new
values. Preserve omitted versus zero values with defaults/pointers where needed.
Reject unsupported field types and invalid tags at descriptor validation.

Use an extension namespace in JSON and crushrc. An illustrative new crushrc form
is `option extensions context-projection keep-recent 5`; it is not current syntax.
Keep existing `option context-projection ...` and `option subagent-model ...`
forms as migration aliases. When old and new paths both occur, define and test
precedence instead of silently choosing a value. Existing config layering and
scope precedence must remain unchanged.

Decode explicitly supported older config versions into the current candidate
before validation. Unsupported newer versions reject activation/reload with a
diagnostic and leave the stored block unchanged. Loading must not persist an
upgrade. Old/new alias conflicts are rejected unless the values agree, after
normalizing them to the same field within each layer.

The implementation needs explicit adapters at all these locations:

- `internal/config/config.go`: data storage, tool validation, defaults, schema
  reflection, and option normalization.
- `internal/config/load.go` and `store.go`: layer merging, snapshots, and writes.
- `internal/shellconfig/options.go`: namespace dispatch and field parsing.
- `internal/cmd/schema.go` and `schema.json`: merge extension schemas, including
  descriptions, defaults, ranges, required fields, and unknown-field policy.

A JSON round trip is a possible clone strategy only for the supported JSON
subset. It is not a general deep-copy implementation for arbitrary `any` values.
Duration tags need matching JSON and shell encodings. Cross-field/model validation
remains executable code. `ModelCatalog` must be an immutable snapshot passed by
the host, not an import back into the concrete config loader.

Reload has a prepare/commit contract:

1. Serialize reloads per workspace and build a candidate config generation.
2. Validate options, prepare feature state, and build all affected tools/prompts.
3. If preparation fails, discard the candidate and retain the active generation.
4. Publish config and runtime state together. Existing turns retain their captured
   generation. New turns use the new generation.
5. Emit one generation-change event after publication.

Do not offer a fallible `OnConfigChange` callback after committing config and
then claim rollback. Persistence and activation failures must be distinguishable.
Config writes use the existing store's selected scope and shell/JSON precedence.
The route returns both persisted and active revisions if activation fails after
a successful write. A file-watch reload failure leaves the last active generation
available and reports the rejected revision. A global-scope write activates in
the addressed workspace only; other workspaces keep their current generation
until their next reload/restart. The response must state that scope of activation.
This matches the existing target-workspace backend reload behavior, without
promising a cross-workspace transaction.

Extension config writes are limited to declared fields in that extension's
namespace. Skills toggle/reload is a narrow host operation because
`disabled_skills` belongs to core and the current UI writes it globally.
Unrecognized extension configuration may be preserved for compatibility, but
cannot become active or be edited through the extension route without a matching
descriptor. Live enablement changes return `restart_required` in version one.

Evidence: `Config` and `Config.Options` are concrete types. `Config.cloneForWrite`
manually clones projection options. `crush schema` reflects `config.Config`.
The present four-argument handler at `shellconfig/options.go` is handwritten.
Tags alone do none of this integration work. The current
`ReloadSkills` calls `Manager.Reload` before preparing replacement tools/prompts,
and constructs tools before updating the coordinator's skill fields. Its sequence
is not the atomic publication contract above. A generation adapter must address
these ordering constraints rather than wrapping the existing method unchanged.

## 5. Turn lifecycle and prompt composition

### 5.1 History transformation

The current hook in `sessionAgent.Run` flushes message writes and calls
`getSessionMessages` before projection. This returns **summary-bounded canonical
history**, not necessarily every stored message. It computes first-user/title
eligibility from that history before applying projection. The new user message
is created afterward (`internal/agent/agent.go`, near lines 718–794).

A consumer-owned transform interface can have this shape:

```go
type TurnInfo struct {
	RunID     string
	SessionID string
	AgentName string
	Subagent  bool
}

var ErrUseCanonical = errors.New("extension: use canonical history")

type HistoryTransform interface {
	Before(context.Context, TurnInfo, []message.Message) ([]message.Message, error)
	After(context.Context, TurnInfo, error) error
}
```

`Before` receives an isolated copy of summary-bounded canonical history. Its
returned messages affect only the provider request. Nested parts, byte slices,
and mutable maps must not alias canonical storage or another observer's input.
The host retains the original snapshot for fallback and title decisions. The
transform must not include the pending user prompt twice or save transformed
history as canonical messages.

For version one, allow at most one history-rewriting contributor per agent.
Read-only observers may compose. Projection validates supplied message IDs
against canonical history, so chaining arbitrary rewriting contributors is not
proven safe. A general chain needs a separate composition design.

Define error behavior explicitly:

- The extension translates a recoverable feature error to `ErrUseCanonical`.
  The host checks it with `errors.Is` and selects the canonical snapshot. The
  host does not import projection's concrete error type.
- Cancellation and all unclassified errors abort execution. Cancellation wins
  over fallback. Never invoke a provider after observing cancellation.
- Arm `After` before calling `Before`, so failed preparation can still account
  usage. Run finalizers in reverse entry order with bounded detached contexts.
- Finalizer errors are reported and retain retryable accounting state. They do
  not erase the primary run error or successful model output.
- Flush messages and complete required accounting before publishing the terminal
  run event. Cover early errors as well as successful streaming exits.

The existing deferred accounting occurs before `Run` returns, but its ordering
relative to the session-agent completion callback relies on Go defer order.
A new host must specify the observable sequence instead of copying defer
placement blindly. Exactly one authoritative completion event belongs to each
accepted run. Provider authentication retries reuse that run ID.

### 5.2 Prompt fragments

Fragments append after the built-in prompt and before provider execution.
They receive agent kind, workspace, and an immutable config/skill snapshot.
A clock-dependent fragment is evaluated for each executing turn, including a
queued turn, rather than only when the agent is constructed. Title,
summarization, and agentic-fetch calls need explicit inclusion/exclusion rules.

Cron currently adds `Time` inside three templates through
`internal/agent/prompt/prompt.go`. Appending the same text changes its position.
Accept that visible change only with prompt tests. Keeping the small time field
in core is a simpler alternative if template placement matters. A generic slot
system is not required by the current evidence.

## 6. Provider construction and auxiliary model calls

### 6.1 Request adaptation

The Codex endpoint patch in `buildOpenaiProvider` uses
`openai.WithSDKOptions(openaisdk.WithJSONDel("max_output_tokens"))`. It covers
callers that use that provider builder, including auxiliary model callers.
It is an SDK request option today, not a custom `RoundTripper`.

A transport adapter is feasible, but moving this one deletion into a framework
is not automatically simpler. Keep the compatibility patch in core, or extract
a narrow OpenAI builder policy first. Generalize across providers only with a
second concrete use and tests for each supported family.

If a transport seam is added, use a fallible construction contract:

```go
type TransportTarget struct {
	ProviderID   string
	ProviderType string
	BaseURL      string
	ModelID      string
	BodyLogging  bool
}

type RequestMiddleware interface {
	Wrap(TransportTarget, http.RoundTripper) (http.RoundTripper, error)
}
```

A construction error or nil transport fails that model build. A request-time
error fails that request. Do not log and silently bypass a required adapter.
The host supplies a non-nil existing transport and preserves the provider's
client, authentication, proxy, retry, timeout, and streaming behavior.

Every builder needs its own adapter. `coordinator.go` has nine named builder
functions, special OpenCode routing, Copilot clients, and custom-provider
fallbacks. Provider identity and effective endpoint must reflect that routing.
A request-body rewrite must occur before signing where applicable. Do not claim
support for every provider merely because all eventually use HTTP.

For the Codex transform, match parsed HTTPS host/path and inspect the actual
request endpoint. Clone request metadata, preserve unrelated JSON numbers and
fields, update body length and replay behavior (`GetBody`), close consumed bodies,
and leave response streams untouched. Test retries, cancellation, unrelated
endpoints, malformed bodies, and auxiliary callers. These follow the
[RoundTripper contract](https://pkg.go.dev/net/http#RoundTripper), not just its
method signature.

### 6.2 Models, options, privacy, and cost

The statement that Fantasy has no provider-generic option type was too broad.
Fantasy v0.41.3 defines `fantasy.ProviderOptions`. Its entries and provider
construction options are family-specific. The current summarizer sets JSON mode
on the **call's** OpenAI-compatible options, not on the built language model.

A host model factory therefore returns a bundle: language model, copied effective
call options, selected model/provider identity, pricing metadata, and flat-rate
status. It accepts the selected model slot or a validated provider/model pair,
call purpose, subagent status, and logging policy. Do not return only
`fantasy.LanguageModel` while expecting the extension to reproduce private
provider resolution and billing rules.

Preserve these current behaviors from `coordinator.go` and
`internal/agent/condense_summary.go`:

- Summarizer construction disables HTTP body logging even in debug mode.
- OpenAI-compatible summarizer calls copy options and request `json_object`.
  Other families currently rely on the prompt contract. A universal
  `JSONMode: true` would promise more than this implementation provides.
- Auxiliary usage includes cache tokens, provider-reported OpenRouter cost, and
  zero cost for flat-rate providers.
- Summarization uses bounded input/output, a timeout, zero automatic retries,
  typed failure classification, sanitized records, and bounded Unicode text.
- Authentication, provider headers, model defaults, and Copilot/subagent behavior
  continue through the host's provider factory.

Prefer exposing the existing model-resolution result through an adapter over
inventing a closed option struct that needs a new field for every provider
feature. Preserve logging policy as an explicit host constraint.

## 7. Tools and delegated agents

### 7.1 Tool declarations and construction

Static tool names and policies must exist before config builds `AllowedTools`
and validates `disabled_tools`. Runtime construction happens later, after
workspace dependencies exist. A tool factory receives agent kind and workspace
services. The current session/message/tool-call IDs come from execution context,
as `context_tree_query` and the agent tool already do. A `SessionID` cannot be
required when tools are built for a reusable agent.

Construct fresh mutable tool wrappers per agent generation. Fantasy tool objects
have mutable provider options. A copied slice alone does not isolate them.
Factories may fail, so their interface must return an error.

The host resolves built-in tools, extension additions/replacements, and MCP tools
through one collision policy. A replacement must name an existing replaceable
built-in and have the same tool identity. Reject duplicate owners, unknown
replacement targets, and attempts to replace reserved recovery names. Apply the
same checks on late MCP discovery and reload. Reject the conflicting update while
retaining the valid registry, rather than exposing an ambiguous name.

Ordinary additions/replacements retain agent allowlists, disabled-tool filtering,
MCP restrictions, hooks, and permission behavior. `SkipHooks` cannot mean
"remove the permission wrapper": `hookedTool` runs PreToolUse, while many tools
perform permission checks inside their implementations. Current delegated tools
also skip PreToolUse by existing policy.

Projection recovery is an explicit infrastructure exception:

- It is registered for the top-level agent whenever the projection module exists,
  including when creation of new projections is disabled.
- Its reserved name cannot be removed by ordinary tool filtering while visible
  references may require it.
- It bypasses deny/rewrite hooks and uses a separate content-free audit wrapper.
- Recovery remains session-scoped, read-only, paginated, and bounds-checked.

Do not expose an unrestricted security-bypass flag as a general feature. Model
this exception as an audited recovery capability. Evidence is
`buildTools`, `appendContextRecoveryTool`, `recoveryAuditTool`, and
`internal/agent/tools/context_tree_query.go` with their tests.

### 7.2 Delegation

Replacing `agent` can own the parameter schema and description. The execution
adapter must accept parent session ID, assistant message ID, tool-call ID,
prompt, agent type, and selected model slot. It must return
`fantasy.ToolResponse`, preserving tool errors and metadata, rather than a plain
string.

The host still owns child session identity, agent construction, prompt selection,
allowed tools, recursion policy, cancellation, authentication, and
parent cost propagation. These responsibilities currently span `agentTool`,
`subagentConfig`, `applySubagentModel`, `buildAgent`, and `runSubAgent`.
A public wrapper around only `runSubAgent` does not supply them.

An alternative is to keep the small delegated-agent selection behavior upstream
in core. Copying the entire built-in agent tool into an extension creates an
upstream synchronization obligation. The extension approach is only a partial
fit until construction and failure isolation have deterministic tests.

The current source still has the readiness problems described in
`HANDOFF-agent-subagent-readiness.md`: `buildAgent` adds asynchronous prompt/tool
initializers to coordinator-wide `readyWg`, but `runSubAgent` executes the returned
agent without waiting for those initializers. Their errors can also affect later
top-level readiness waits. The adapter must await per-invocation initialization
and isolate its failure from other agents.

`subagentConfig` currently falls back to the task agent for an unrecognized type.
The default general-purpose configuration also retains the `agent` tool, allowing
nested delegation. Validate type names explicitly. Whether to exclude nesting or
introduce a depth/budget limit remains a product decision before this adapter is
considered complete. These are existing implementation issues, not functionality
that a replacement tool automatically fixes. No implementation change is proposed
as part of this documentation audit.

## 8. Background services and prompt dispatch

A service receives its dependencies at construction and runs after Ready:

```go
type Service interface {
	Run(context.Context) error
}
```

`Run` blocks until shutdown or failure. Child goroutines must be tracked and joined
before it returns. Returning nil unexpectedly does not automatically restart a
service. Record stopped/failed state and apply an explicit retry policy owned by
the feature. The host must not hold a registry/config mutex while a worker runs.

Recovering a panic around `Run` catches only that goroutine. It cannot catch a
panic in an independently started child goroutine. Registered entry points need
panic handling with the same failure semantics as returned errors. Extensions
must guard/join their child workers or use host-managed worker execution.
Recovery is containment, not proof that the extension remains usable. Stop new
activity for the failed instance. If integrity/recovery behavior is compromised,
fail the affected operation/workspace instead of deleting its hooks and continuing.
See [Go panic and recovery](https://go.dev/ref/spec#Handling_panics).

Cron needs both prompt admission and the terminal execution result. A suitable
shape is:

```go
type RunOutcome uint8

const (
	RunSucceeded RunOutcome = iota + 1
	RunFailed
	RunCancelled
)

type RunResult struct {
	RunID     string
	SessionID string
	Outcome   RunOutcome
	Err       error
}

type RunHandle interface {
	ID() string
	Wait(context.Context) (RunResult, error)
	Cancel()
}
```

A dispatch operation returns `(RunHandle, error)`: the error reports admission
failure, while `Wait` reports execution. Waiting cancellation stops the wait;
`Cancel` cancels that accepted run, not an unrelated run on the session. The handle
retains its immutable terminal result until the caller drops the handle. Remove
settled runs from the host's active-run index, so the host does not retain an
unbounded completion history. A caller holding the handle can wait repeatedly.
Events carry the same workspace/session/run IDs and are a notification mechanism,
not the sole source of correctness.

Register acceptance and its run ID before scheduling execution. Queued service
prompts must remain separate turns. Current prompts without a run ID can be
folded into an active step in `agent.go`, whereas identified runs preserve their
own lifecycle. Define queue capacity and return `queue_full` without admission
when it is exhausted. Once admitted, a run settles exactly once on execution,
cancellation, deletion, or shutdown. The worker owns the run until settlement.
Its accepted execution is tied to the workspace/extension context, not a UI client's connection. A client disconnect
does not cancel scheduled work. Admission uses the dispatch call's context;
after admission, cancellation uses the handle or its owning lifecycle context.

The cron callback waits for the handle and returns the actual run error. Its own
serialization may remain, but it must not share a lock with config or skill
reload. Current `scheduler.Tick` calls `MarkFired` or `MarkError` based on the
callback result (`internal/scheduler/ticker.go`). The new contract uses mutually
exclusive success, failure, and cancellation outcomes. Success has nil `Err`.
Failure and cancellation have a non-nil error/cause. A cancelled result must never
be counted as a successful fire.

The cron adapter maps success to `MarkFired`, execution failure to `MarkError`,
and confirmed session deletion to `DropSession`. Feature/workspace shutdown
cancellation releases the in-flight claim without recording a success or a new
retry error. Other explicit run cancellation records a cancelled attempt through
the error path. These shutdown rules require a ticker adapter change and tests;
the existing ticker only sees an error and does not implement this distinction.

Commit `5205e8ac` changed the callback from fire-and-forget to waiting for
completion. It did **not** fix a
skill reload blocked by that cron mutex. `coordinator_cron_test.go` explicitly
checks that fire does not return early and preserves the run error.

Cron also needs workspace data-directory access and a session-existence lookup.
Drop tasks only for a confirmed missing session, not a transient database error.
Retain lazy cleanup at fire time or use a post-commit session-deleted event. Do not
remove durable JSON tasks from a database before-delete callback, because the
transaction may roll back.

Preserve task persistence, recurrence, jitter, retries, expiry, and deletion
rules from `internal/scheduler`, except for the explicitly proposed outcome
changes above. Enqueue success is not task success.

## 9. Storage, migrations, and session lifecycle

### 9.1 Transaction capability

The original callback accepted only `*db.Queries`. That type wraps an unexported
connection field. It cannot construct an extension's independent sqlc query set.
Pass a transaction-bound query capability instead:

```go
type DBTX interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	PrepareContext(context.Context, string) (*sql.Stmt, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
```

The host supplies a wrapper over the active transaction with no Begin, Commit,
or Rollback methods. Both `db.New(tx)` and extension-generated `New(tx)` can use
it structurally. The capability expires when the callback returns. Do not retain
it or start goroutines using it. Close query rows before callback return.

Callbacks may use only transaction-bound database capabilities. They must not
call host services, perform network work, wait for workers, start another
transaction, publish events, or acquire a lock held by code waiting for this
transaction. `db.Connect` sets `MaxOpenConns(1)`: even a second pool **read** can
block behind the open transaction. This is not merely a SQLite write-lock issue.
Context checks can catch some re-entry mistakes, but cannot guarantee detection
when trusted code retains another database handle. Do not promise a complete
debug-build guard.

New feature queries and generated code live with the feature. Existing core
queries are reusable through transaction-bound adapters. Raw writes that bypass
core lifecycle events are not a supported host service. Core schema changes
still need a core migration and compatibility review.

### 9.2 Delete and copy sequence

Core owns canonical session/message mutation and its transaction. Workspaces
sharing a pooled database must share lifecycle coordination for that database.
A barrier private to one workspace cannot protect writes from another workspace.
The session fork command must call that operation, not reimplement copy SQL in an extension.
The required sequence is:

1. Enter a per-database/session lifecycle state shared with dispatch. Reject new
   turn admission and serialize competing copy/delete requests.
2. For deletion, cancel and settle active and queued run handles. For copying,
   hold queued runs and await the active run under the caller's deadline. Allow
   that active run to finish writing while it settles. If waiting fails, restore
   admission without starting a storage transaction.
3. Establish the message-write barrier. Wait for accepted writes. Flush a copy
   snapshot before beginning its transaction. Apply the existing delete rules
   for pending buffered writes.
4. Begin the core transaction. Run ordered before-mutation callbacks.
5. Perform the canonical mutation. For copies, allocate fresh message IDs and
   produce an old-to-new message-ID map.
6. Run after-mutation callbacks with transaction-bound access and the map.
7. Commit, or roll back on any callback/core error or recovered panic.
8. Release write/lifecycle barriers on every exit. Resume held source runs after
   copy or failed mutation, but never after successful deletion. Publish core and
   extension invalidation events only after commit, outside locks/transactions.

Waiting for an active turn before copying changes when a busy-session fork
completes. It is a proposed behavior, not current behavior proven by the existing
message barriers. Validate that UX explicitly. Keeping a snapshot during active
streaming instead would require a separate protocol to buffer/resume source
writes without failing the active turn. Neither behavior follows from a storage
observer alone.

Do not flush or wait for message writers inside a callback. Current snapshot and
delete barriers live in `message.Service`, with callers in backend, workspace,
app, and CLI code. `session.Service.Delete` opens its transaction afterward.

Callbacks cover session deletion, canonical message deletion, and session copy.
Copy callbacks receive source/destination IDs and the message-ID map. Projection's
current fork policy copies canonical messages and summary/todo/usage metadata,
but starts with fresh derived projection state. It does not clone recovery refs
or unaccounted usage attempts. Other feature tables must explicitly choose fresh,
copy-with-remapping, or reject-copy behavior.

`CreateMessageAt`, timestamp preservation, summary-ID remapping, estimated usage,
and session events are core copy responsibilities. The compound unique index on
`messages(session_id, id)` supports projection's foreign keys. Deterministic
message ordering is another core requirement. Neither becomes extension-owned
simply by moving the migration file.

### 9.3 Atomic usage accounting

`condense.Store.AccountUsage` marks each attempt accounted and increments session
usage in the same transaction. Replacing this with `Host.AddUsage` followed by an
extension update permits double charging after a crash or retry.

Provide a transaction operation that gives the feature both its `DBTX` and a
transaction-bound session usage accumulator. The feature consumes its durable
attempt ledger and increments usage in that transaction. The accumulator checks
finite/nonnegative cost, overflow, and session existence. Publish session/state
invalidation only after commit. Keep the ledger idempotent across retries,
cancellation, invalid summaries, and restart.

This is an explicit core write capability. General access to every core query
is broader than required and does not preserve service invariants by itself.

### 9.4 Migration ownership and upgrades

Current `db.Connect` configures one embedded filesystem and a global Goose runner.
Do not merge unrelated extension versions into the core migration ledger or
switch process-wide Goose globals while workspaces open concurrently.

Use an isolated Goose provider per extension with a stable, validated ledger
name. Goose v3.27.3 supports `NewProvider` and `WithTableName`. Supply each
extension's migration directory as the provider filesystem root. Run core first,
then extension migrations in dependency order under database-open serialization.
See [Goose provider documentation](https://pressly.github.io/goose/documentation/provider/).

Run migrations for all compiled descriptors, independent of runtime enablement.
Fail database activation on a migration error. Already committed migrations are
not undone automatically. Rerunning must resume safely. Reject unsupported newer
schema versions. Opening the same pooled database must not rerun competing
migration sequences. Read-only stats collection runs no migrations.

The existing fork already applied
`20260226000000_add_context_projection.sql` through the core ledger. Migration
extraction therefore needs an adoption plan: retain historical core migration
compatibility, verify the existing tables/indexes, seed the extension baseline
without recreating tables, and run future extension changes in its own ledger.
Test both fresh and already-upgraded databases. Do not move or renumber applied
migrations and assume old installations will work.

Required storage adapters must remain installed while their data exists. Removing
a compiled extension with foreign keys into core needs an explicit uninstall or
compatibility migration. Config disablement alone is not schema removal.

## 10. Commands, transport, UI, and statistics

### 10.1 Commands and wire contract

Command handlers execute in the backend workspace. Each command declares a
namespaced ID, input/output schema, scope, availability conditions, and a typed
JSON result. CLI argument parsing and UI interaction are adapters, not inferred
from `Run(ctx, args json.RawMessage)`.

Discovery advertises the supported wire version and available surfaces for each
extension. Version one requires an exact version match before activating its
client adapter. An incompatible client can show an unsupported status, but cannot
invoke extension commands/config writes. Add broader negotiation only with an
explicit compatibility mapping.

A command may have CLI, palette, or remote exposure independently. A dialog-open
action is client-local. Invoking a skill populates the editor in the current UI
flow and must not silently become a server command that runs a prompt. Preserve
`crush session fork <id> --json`, ID-prefix resolution, and its output contract via
an explicit CLI adapter or keep that core command.

The shared protocol requires capability discovery as well as operations:

```text
GET  /v1/workspaces/{id}/extensions
GET  /v1/workspaces/{id}/extensions/{ext}/state?session_id=...
POST /v1/workspaces/{id}/extensions/{ext}/commands/{name}
POST /v1/workspaces/{id}/extensions/{ext}/config
```

An extension state/event envelope contains wire version, extension ID, workspace
ID, optional session ID, generation/revision, and JSON payload. The server validates
JSON serializability, payload limits, scope, and command/field availability before
invocation. Bind workspace and extension identity from the authenticated route,
not from caller-provided payload fields. Command errors distinguish unknown,
disabled, invalid input, conflict, not ready, and execution failure. Use existing authentication/workspace routing.
Do not expose raw database handles, Go objects, provider secrets, or arbitrary
config paths over the wire.

`SetState(any)` is insufficient: state must be immutable, keyed by workspace and
session, serializable, and revisioned. Return a marshal error to the publisher.
Store a copy, not a mutable caller-owned pointer. The state route supplies a
snapshot. A registered generic SSE payload supplies invalidations or newer
snapshots. Reconnects, missed events, and session switches refetch state.
Reject stale responses from a previous session or generation.

`internal/server/events.go` wraps a fixed set of event types, and
`internal/client/proto.go` decodes a fixed set. Add an extension envelope to both
directions, register request handling in `internal/server/proto.go`, and extend the
UI dispatcher/cache. Unknown extension versions show unavailable/unsupported
state without crashing the client. Generic routes remove repeated route code,
not extension-specific JSON schemas or compatibility work.

Current remote UI parity is incomplete: `ClientWorkspace.AgentListCronTasks`
returns nil and `AgentSessionProjectionStats` returns zero because their remote
protocols do not exist. These are new capabilities to prove, not existing behavior
to preserve merely by renaming routes.

For session fork, return the new session ID and let the invoking client navigate.
A broadcast session-created event must not force every connected client to switch
sessions. Current UI fork navigation is explicitly handled in `ui.go`.

### 10.2 UI contracts

Separate optional UI contributors: status rows, pills, dialogs, tool renderers,
and palette entries. Do not require every extension to implement a large interface
with meaningless methods. State decoding occurs at the extension's presentation
adapter. The UI receives immutable snapshots and explicit loading/error states.
No synchronous database or HTTP call belongs in a render function.

| Surface | Host seam and contract |
|---|---|
| Status rows | `ui/common/elements.go`, `ui/model/sidebar.go`, and compact details in `ui.go`. Stable row IDs, priority, width/height budget, theme tokens, and overflow rules. |
| Pills | `ui/model/pills.go`. Stable IDs, ordering, layout budget, action binding, and refresh invalidation. |
| Dialogs | `ui/dialog` routing and `ui/model/ui.go`. Construct with dimensions, theme, key bindings, focus/close rules, and a client command adapter. Route results back to the owning instance. |
| Tool renderers | `ui/chat/tools.go`. Consume tool call/result, width, theme, lifecycle state, and renderer invalidation. Fall back to the core renderer on optional-renderer failure. |
| Palette | `ui/dialog/commands.go` and actions. Dynamic discovery after skill/config reload, stable command IDs, availability, selection, and editor actions. |
| Events/cache | `ui/model/workspace_cache.go` and dispatcher. Workspace/session/version checks, async fetch, reconnect refresh, teardown on workspace close. |

Follow `internal/ui/AGENTS.md`: the main UI remains the sole Bubble Tea model.
Dialog adapters implement the existing imperative `ID`, `HandleMsg`, and `Draw`
contract, rather than introducing nested Bubble Tea models. State mutation occurs
on the UI update loop. Run I/O in `tea.Cmd` and return messages. Adapters cannot
mutate shared UI state from arbitrary goroutines.
Backend instances and UI instances are separate even in local mode.

The existing projection row combines canonical session data, provider usage, and
projection state. A single unlabeled string loses that meaning. Preserve its
placement under context usage, compact-overlay parity, loading behavior, and
zero-state rules with the existing render tests. The dirty sidebar edits are not
part of this extraction audit.

The skills dialog also needs theme styles, keyboard help, focus handling, toggle
results, and refreshed skill snapshots. Generic routes do not supply those.
Keep discovery/deduplication and the reloadable skills manager in core. Expose
snapshot/reload/toggle operations to the UI adapter. Refresh invocable builtin
palette entries from the new snapshot rather than registering them only at Init.

### 10.3 Statistics

`crush stats` opens historical project databases read-only via
`db.ConnectReadOnly`; it does not construct the live app. A runtime-only `Stats`
claim cannot serve this path. A static collector factory receives the read-only
handle and database schema information. It returns typed sections with stable
metric keys, labels, units, and aggregation rules. An absent extension table means
unavailable data, not necessarily zero.

Use the same numeric data for CLI and the dashboard's JSON payload. Compute
compression from summed raw/projected characters, not the average of per-session percentages.
Keep paid summarizer cost separate from savings. Preserve the existing projection
fields in the dashboard JSON or version that payload. A generic HTML table needs
renderer and payload changes in `internal/cmd/stats/index.html` and `index.js`; labels and values alone
are insufficient for aggregation and existing compatibility.

## 11. Feature coverage and classification

“Partial” means the feature logic can move after the named host behavior exists.
It does not mean the original interface already implements that behavior.

| Fork feature | Classification and evidence | Required integration |
|---|---|---|
| Context projection and recovery | Partial. `internal/condense`, `condense_summary.go`, projection agent/backend tests. | Turn snapshot/finalization, model bundle, options, recovery tools, migrations, transactions/accounting, state/UI/stats. Keep canonical history and lifecycle guards in core. |
| `context_tree_query` | Partial. Tool and `hooked_tool.go` tests verify reservation, read bounds, and audit. | Recovery-specific policy, session context, late-collision checks, availability after projection disable. |
| Cron | Partial. Scheduler/store/ticker, three tools, coordinator cron tests, chat renderer tests. | Workspace service, terminal run handle, session/path access, prompt time, tools, pills/renderers, durable file lifecycle. |
| Session forking | Core operation plus extensible presentation. Session, backend/workspace/CLI fork tests. | Core snapshot barrier and atomic copy; command adapters and optional copy observers. Preserve canonical timestamps and summary mapping. |
| Codex endpoint compatibility | Small provider policy, technically extractable. `buildOpenaiProvider` and commit `fa831171`; dedicated transport regression tests remain required. | Existing SDK deletion option is the smaller starting point. General transport registry is deferred. |
| Delegated model/type selection | Partial, or keep upstream in core. `agent_tool.go`, model/tool tests, readiness handoff. | Full delegated construction/execution adapter, tool schema, config default, recursion/permission/cost semantics. |
| Skills discovery/reload | Core behavior. `skills/manager.go`, discovery and manager tests, backend config. | Immutable snapshots, reload serialization, prompt/tool refresh, workspace event propagation. |
| Skills dialog and builtin palette | Partial presentation extraction. `ui/dialog/skills.go`, UI actions/styles, builtin frontmatter. | Client presentation factory, dynamic commands, toggle/reload adapter, theme/key/focus support. |
| Loop prototype | Superseded by cron. Introduced in `6ff260d6`, removed in `bedfe54a`. | No live interval scheduler to extract. Preserve surviving fork command and builtin fork skill. |
| Message order, codec, write barriers | Required core support. SQL, message/session code and regression tests. | Stable ordering, composite FK target, exported canonical decoding, quiescence outside transactions. |
| UTF-8 paging helper | Shared leaf utility. `internal/stringext/utf8.go` and tests. | No extension registry needed. |
| Docs, instructions, ignores, cassettes | Supporting artifacts or unrelated housekeeping. | Keep instructions/docs with their features. Tests and recorded fixtures are not host seams. |

## 12. Core cost and smaller alternatives

The previous “roughly 650 lines” estimate is withdrawn. It counted call sites
while omitting registry implementation, database migration/adoption, shutdown,
config publication, transaction accounting, queue correlation, client activation,
wire/cache types, CLI parsing, read-only stats, and verification. Even its own
“15 lines per provider family” allowance was not included consistently in the
sum. No implemented prototype supports a replacement total.

The measured fork size is 17,816 insertions and 483 deletions across 153 files.
It includes feature implementation, tests, generated SQL, documentation, and
binary recordings. `internal/db/context_projection.sql.go` alone has 1,391
lines. Moving those queries does not remove them, and it is not evidence that
the host framework is smaller by the full fork-diff size.

| Required host work | Concrete source locations | Feature-owned code after extraction |
|---|---|---|
| Composition and workspace lifecycle | `app/app.go`, `backend/backend.go`, local CLI setup | Descriptor/factories, feature state, worker cleanup. |
| Config/schema/tool manifest/reload | `config/{config,load,store}.go`, `shellconfig/options.go`, `cmd/schema.go` | Option struct/validation/schema contributions. |
| Migration plans and adoption | `db/connect.go`, historical core migration | New feature SQL, independent sqlc output, version ledger. |
| Turn history/finalization | `agent/agent.go`, accepted-run completion paths | Projection algorithm, summarizer, durable attempt ledger. |
| Model/provider adapter | `agent/coordinator.go` builders/options | Provider-specific policy if extraction is justified. |
| Tool registry and policies | `agent/coordinator.go`, `hooked_tool.go`, config tool lists | Tool schema, constructor, implementation, renderer. |
| Dispatch and delegated execution | `agent/agent.go`, `coordinator.go`, `agent_tool.go` | Cron policy and any replacement delegation schema. |
| Canonical mutation/accounting | `message/message.go`, `session/session.go`, backend/workspace/app/CLI callers | Transaction-bound derived-data callbacks. |
| Commands and protocol | `cmd/root.go`, `cmd/session.go`, server/client proto, workspace adapters | Command inputs/results and feature handlers. |
| UI composition/cache | `ui/model`, `ui/common`, `ui/chat`, `ui/dialog`, styles | Feature dialogs, pills, renderers, state decoders. |
| Stats collection/output | `cmd/stats.go`, read-only DB path, stats HTML/JS | Collector and typed metric definitions. |
| Verification | Existing package/unit/integration/render tests | Feature tests plus cross-feature registry/lifecycle tests. |

Potential independent upstream changes are canonical ordering, the composite
message key required for foreign keys, snapshot/delete barriers, atomic session
usage operations, canonical copy/timestamp support, and skills reload semantics.
Provider compatibility and delegated selection may also be cheaper upstream
patches than extension capabilities. These are recommendations, not predictions
of upstream acceptance. The local PR inventory is historical and was not refreshed.

Keep the existing narrow interfaces (`contextProjector`, message deletion adapter,
session delete lifecycle, `scheduler.FireFunc`) while proving extraction. A
universal Host with raw DB access, all generated queries, agent construction,
commands, config writes, UI state, and events recreates application coupling under
a different package name. Prefer smaller constructor dependencies for each feature.

## 13. Build order and verification gates

1. Keep core integrity changes explicit. Record baseline feature behavior and
   build a compile-only dependency prototype with two isolated workspaces.
2. Prove static descriptors, tool/config validation, instance ownership, and
   separate presentation activation. No mutable process singleton.
3. Prove migrations/adoption, canonical lifecycle barriers, transaction capability,
   and idempotent usage before moving projection storage.
4. Extract projection as one vertical feature: history, models/privacy, recovery,
   config, persistence, state, and stats. Leave incomplete surfaces in core until
   their replacements pass compatibility tests.
5. Introduce run handles and migrate cron only after scheduler success/error,
   queued-turn, cancellation, and shutdown tests pass.
6. Add generic command/state/config adapters and client capability discovery with
   local/remote parity tests. Keep core fork mutation behind the command adapter.
7. Extract presentation incrementally, validating sidebar/overlay, pills, tool
   rendering, skills focus/toggle/reload, and dynamic palette entries.
8. Add provider middleware or tool replacement only where the narrower alternative
   demonstrably fails to cover real requirements.

Each stage must preserve working behavior. The old order moved projection before
storage hooks, and moved cron onto enqueue-only dispatch. Those stages could not
claim compatibility independently.

Required verification includes:

- Compile/import-cycle checks and independent instances in two workspaces.
- Deterministic registration, conflict handling, partial-init rollback, and reload
  generation isolation under concurrent turns.
- Fresh/legacy/disabled-feature database upgrades, rollback on callback failure,
  no transaction re-entry, copy/delete barriers, and no duplicated usage.
- Canonical fallback/cancellation, title and summary boundaries, recovery after
  disable, reservation on late MCP registration, and sanitized diagnostics.
- Correlated queued execution, terminal errors, recurrence/retry outcomes,
  service cancellation, panic handling, and shutdown before DB release.
- Provider construction/call options, no summary-body logging, HTTP request replay
  if transport rewriting is used, and unchanged unrelated provider requests.
- Local/client parity, state revision races, unsupported versions, reconnects,
  command/config validation, and UI navigation limited to the invoking client.
- Historical read-only stats with missing tables and correct aggregation.

This audit changes documentation only. It does not establish that an extension
implementation passes these gates.

Audit validation on the current checkout:

- `go test ./internal/agent ./internal/scheduler ./internal/session ./internal/message ./internal/backend ./internal/workspace` passed.
- The five Go declaration sketches were formatted and compiled together in a
  temporary module against this checkout. The transaction interface was also
  checked for compatibility with `db.DBTX`.
- Documentation checks cover fences, whitespace, the complete 20-commit ledger,
  all 153 distinct changed paths, and unchanged hashes for the two pre-existing
  dirty UI files.

These checks validate the examples and current implementation evidence. They do
not compile a full extension host or prove the proposed concurrency contracts.

## 14. Commit and file-group audit ledger

The following ledger accounts for all commits in the comparison. Follow-up fixes
remain requirements of the owning feature rather than separate capabilities.

| Commit | Scope and disposition |
|---|---|
| `6ff260d6` | Session fork plus loop prototype. Core copy/CLI/UI adapters survive; loop is superseded by cron. |
| `fa831171` | Codex request-field compatibility. Narrow provider policy. |
| `e309d374` | Builtin skills made user-invocable, including fork. Dynamic palette and feature documentation. |
| `e0e3b9d8` | Consistent/reloadable skill discovery. Core manager, snapshots, and propagation. |
| `dc2ed474` | Interactive skills dialog. UI/client/config adapters plus theme work. |
| `678d197f` | `subagent_type`. Delegated agent construction. |
| `bedfe54a` | Cron store/tools/UI/tests, prompt time, dependency, and loop removal. Service/dispatch feature. |
| `7bb40534` | Delegated model choice/default. Config and agent construction. |
| `5205e8ac` | Serialized skill reload and synchronous scheduled execution outcomes. Preserve tested behavior. |
| `b5ac431a` | Ignore local Pi state. Unrelated housekeeping. |
| `b42342d4` | Projection/recovery implementation, persistence, config, transport/UI/stats, tests, supporting docs, and the separate delegated-readiness handoff. Partial extraction with core integrity requirements. |
| `69a246aa` | Updated TestCoderAgent cassettes. Test maintenance, not an extension seam. |
| `4ea01c34` | Projection savings in details panel. UI state/render contract. |
| `4f8ac903` | Clarify crushrc instructions. Documentation correction. |
| `7798ee24` | Hash binary/image URL/shell content. Projection claim-integrity requirement and codec tests. |
| `04a651c8` | JSON mode for compatible summarizers. Per-call provider options. |
| `1dd42989` | Sanitized summary failure diagnostics. Feature logging contract. |
| `62ab91b9` | Bound overlong summary text. Feature parsing/Unicode behavior. |
| `a2c75bc6` | Projection savings in compact overlay. UI parity. |
| `904a0afb` | Place projection under context usage. UI ordering. |

The file-group ledger below covers production, generated code, tests, docs, and
fixtures. Paths are relative to this repository. Each changed path belongs to
one primary group; cross-cutting files also implement requirements named above.

| Primary group | Files | Disposition |
|---|---:|---|
| Instructions and housekeeping | 5 | Root/UI/skill instructions, separate readiness handoff, and local-state ignore. |
| Configuration | 11 | Defaults, validation, clones, persistence, crushrc parser, schema, docs, and tests. |
| Dependency manifests | 2 | Cron parser dependency and checksums. |
| Agent integration | 14 | Turn/provider/delegation/tool/reload adapters, summarizer, coordinator fake, and tests. |
| Prompt assembly | 7 | Time/skills composition, delegated-tool description, templates, and tests. |
| Recorded provider fixtures | 13 | 13 binary TestCoderAgent cassettes; prompt/test maintenance, not host code. |
| Recovery tool | 3 | Schema, description, session-scoped tool, and tests. |
| Cron tools | 5 | Three tool descriptions, implementation, and tests. |
| Application lifecycle | 3 | Projection wiring, session lifecycle, and tests. |
| Workspace and transport | 19 | Local/server/client adapters, protocol, lifecycle, and test fakes/integration tests. |
| CLI and commands | 4 | Session fork/lifecycle, root registration, skill invocation tests. |
| Statistics | 4 | Read-only collection, CLI/dashboard JSON and rendering, and tests. |
| Projection domain | 17 | Projection, claims, hashing, recovery, SQL orchestration, and feature tests. |
| Database schema and queries | 10 | Projection tables/queries, generated sqlc, message order/timestamp SQL, and stats SQL. |
| Canonical messages and sessions | 5 | Copy/delete/write barriers, codec, accounting, and regression tests; required core support. |
| Scheduler domain | 5 | Cron parsing, persistent store, ticker, and tests. |
| Skills and builtin content | 8 | Core discovery/manager, tests, builtin invocation metadata and fork/config docs. |
| Shared UTF-8 utility | 2 | Leaf paging helper and tests; no extension seam. |
| UI integration | 16 | Cron renderers, projection status/overlay, skills dialog, palette, styles, cache, and tests. |

Total: 153 paths. The exact membership is recorded below to make this
coverage check reproducible. Shared integration files are counted only once.

<details><summary>Instructions and housekeeping — 5 files</summary>

- `.agents/skills/builtin-skills/SKILL.md`
- `.gitignore`
- `AGENTS.md`
- `HANDOFF-agent-subagent-readiness.md`
- `internal/ui/AGENTS.md`

</details>

<details><summary>Configuration — 11 files</summary>

- `docs/config/README.md`
- `internal/config/clone_test.go`
- `internal/config/config.go`
- `internal/config/context_projection_test.go`
- `internal/config/load.go`
- `internal/config/load_test.go`
- `internal/config/shellconfig_option_test.go`
- `internal/config/store.go`
- `internal/shellconfig/options.go`
- `internal/shellconfig/options_test.go`
- `schema.json`

</details>

<details><summary>Dependency manifests — 2 files</summary>

- `go.mod`
- `go.sum`

</details>

<details><summary>Agent integration — 14 files</summary>

- `internal/agent/agent.go`
- `internal/agent/agent_model_test.go`
- `internal/agent/agent_tool.go`
- `internal/agent/agent_tool_test.go`
- `internal/agent/agentic_fetch_tool.go`
- `internal/agent/agenttest/coordinator.go`
- `internal/agent/condense_summary.go`
- `internal/agent/condense_summary_test.go`
- `internal/agent/context_projection_test.go`
- `internal/agent/coordinator.go`
- `internal/agent/coordinator_cron_test.go`
- `internal/agent/coordinator_test.go`
- `internal/agent/hooked_tool.go`
- `internal/agent/hooked_tool_test.go`

</details>

<details><summary>Prompt assembly — 7 files</summary>

- `internal/agent/prompt/prompt.go`
- `internal/agent/prompt/prompt_test.go`
- `internal/agent/prompts.go`
- `internal/agent/templates/agent_tool.md`
- `internal/agent/templates/agentic_fetch_prompt.md.tpl`
- `internal/agent/templates/coder.md.tpl`
- `internal/agent/templates/task.md.tpl`

</details>

<details><summary>Recorded provider fixtures — 13 files</summary>

- `internal/agent/testdata/TestCoderAgent/deepseek-v4/bash_tool.yaml`
- `internal/agent/testdata/TestCoderAgent/deepseek-v4/download_tool.yaml`
- `internal/agent/testdata/TestCoderAgent/deepseek-v4/fetch_tool.yaml`
- `internal/agent/testdata/TestCoderAgent/deepseek-v4/glob_tool.yaml`
- `internal/agent/testdata/TestCoderAgent/deepseek-v4/grep_tool.yaml`
- `internal/agent/testdata/TestCoderAgent/deepseek-v4/ls_tool.yaml`
- `internal/agent/testdata/TestCoderAgent/deepseek-v4/multiedit_tool.yaml`
- `internal/agent/testdata/TestCoderAgent/deepseek-v4/parallel_tool_calls.yaml`
- `internal/agent/testdata/TestCoderAgent/deepseek-v4/read_a_file.yaml`
- `internal/agent/testdata/TestCoderAgent/deepseek-v4/simple_test.yaml`
- `internal/agent/testdata/TestCoderAgent/deepseek-v4/sourcegraph_tool.yaml`
- `internal/agent/testdata/TestCoderAgent/deepseek-v4/update_a_file.yaml`
- `internal/agent/testdata/TestCoderAgent/deepseek-v4/write_tool.yaml`

</details>

<details><summary>Recovery tool — 3 files</summary>

- `internal/agent/tools/context_tree_query.go`
- `internal/agent/tools/context_tree_query.md`
- `internal/agent/tools/context_tree_query_test.go`

</details>

<details><summary>Cron tools — 5 files</summary>

- `internal/agent/tools/cron.go`
- `internal/agent/tools/cron_test.go`
- `internal/agent/tools/croncreate.md`
- `internal/agent/tools/crondelete.md`
- `internal/agent/tools/cronlist.md`

</details>

<details><summary>Application lifecycle — 3 files</summary>

- `internal/app/app.go`
- `internal/app/app_test.go`
- `internal/app/resolve_session_test.go`

</details>

<details><summary>Workspace and transport — 19 files</summary>

- `internal/backend/agent.go`
- `internal/backend/agent_runcomplete_test.go`
- `internal/backend/agent_test.go`
- `internal/backend/backend.go`
- `internal/backend/config.go`
- `internal/backend/context_projection_e2e_test.go`
- `internal/backend/session.go`
- `internal/backend/session_delete_test.go`
- `internal/backend/session_fork_test.go`
- `internal/client/proto.go`
- `internal/server/agent_cancel_test.go`
- `internal/server/e2e_agent_test.go`
- `internal/server/proto.go`
- `internal/server/server.go`
- `internal/server/sessions_isbusy_test.go`
- `internal/workspace/app_workspace.go`
- `internal/workspace/app_workspace_delete_test.go`
- `internal/workspace/client_workspace.go`
- `internal/workspace/workspace.go`

</details>

<details><summary>CLI and commands — 4 files</summary>

- `internal/cmd/root.go`
- `internal/cmd/session.go`
- `internal/cmd/session_lifecycle_test.go`
- `internal/commands/commands_test.go`

</details>

<details><summary>Statistics — 4 files</summary>

- `internal/cmd/stats.go`
- `internal/cmd/stats/index.html`
- `internal/cmd/stats/index.js`
- `internal/cmd/stats_test.go`

</details>

<details><summary>Projection domain — 17 files</summary>

- `internal/condense/condense.go`
- `internal/condense/hash.go`
- `internal/condense/hash_test.go`
- `internal/condense/project.go`
- `internal/condense/project_test.go`
- `internal/condense/project_unit_test.go`
- `internal/condense/query.go`
- `internal/condense/query_test.go`
- `internal/condense/regression_test.go`
- `internal/condense/requirements_test.go`
- `internal/condense/scan.go`
- `internal/condense/scan_test.go`
- `internal/condense/service.go`
- `internal/condense/service_test.go`
- `internal/condense/store.go`
- `internal/condense/store_test.go`
- `internal/condense/test_helpers_test.go`

</details>

<details><summary>Database schema and queries — 10 files</summary>

- `internal/db/context_projection.sql.go`
- `internal/db/db.go`
- `internal/db/messages.sql.go`
- `internal/db/migrations/20260226000000_add_context_projection.sql`
- `internal/db/models.go`
- `internal/db/querier.go`
- `internal/db/sql/context_projection.sql`
- `internal/db/sql/messages.sql`
- `internal/db/sql/stats.sql`
- `internal/db/stats.sql.go`

</details>

<details><summary>Canonical messages and sessions — 5 files</summary>

- `internal/message/codec_test.go`
- `internal/message/message.go`
- `internal/message/message_test.go`
- `internal/session/session.go`
- `internal/session/session_test.go`

</details>

<details><summary>Scheduler domain — 5 files</summary>

- `internal/scheduler/cron.go`
- `internal/scheduler/cron_test.go`
- `internal/scheduler/scheduler.go`
- `internal/scheduler/scheduler_test.go`
- `internal/scheduler/ticker.go`

</details>

<details><summary>Skills and builtin content — 8 files</summary>

- `internal/skills/builtin/crush-config/SKILL.md`
- `internal/skills/builtin/crush-hooks/SKILL.md`
- `internal/skills/builtin/fork/SKILL.md`
- `internal/skills/builtin/jq/SKILL.md`
- `internal/skills/manager.go`
- `internal/skills/manager_test.go`
- `internal/skills/skills.go`
- `internal/skills/skills_test.go`

</details>

<details><summary>Shared UTF-8 utility — 2 files</summary>

- `internal/stringext/utf8.go`
- `internal/stringext/utf8_test.go`

</details>

<details><summary>UI integration — 16 files</summary>

- `internal/ui/chat/cron.go`
- `internal/ui/chat/cron_test.go`
- `internal/ui/chat/tools.go`
- `internal/ui/common/elements.go`
- `internal/ui/common/elements_test.go`
- `internal/ui/dialog/actions.go`
- `internal/ui/dialog/commands.go`
- `internal/ui/dialog/skills.go`
- `internal/ui/model/pills.go`
- `internal/ui/model/session_busy_test.go`
- `internal/ui/model/sidebar.go`
- `internal/ui/model/sidebar_projection_test.go`
- `internal/ui/model/ui.go`
- `internal/ui/model/workspace_cache.go`
- `internal/ui/styles/quickstyle.go`
- `internal/ui/styles/styles.go`

</details>
