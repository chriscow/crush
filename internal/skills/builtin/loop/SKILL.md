---
name: loop
description:
  Schedule repetitive prompts during an active session. Use when the user asks
  to loop, repeat, schedule, or periodically check something. Supports intervals
  (5m, 30m, 2h) or runs with dynamic timing when no interval is provided.
user-invocable: true
---

# Loop Scheduler

Schedule prompts to run periodically during an active session. Loops fire while
the session is open and stop automatically when the session closes or after 7 days.

## Usage Patterns

### 1. Interval + Prompt

Schedule a fixed interval and prompt:

```
/loop 5m check if the deployment finished
/loop 30m review PR #1234
/loop 2h sync the database
```

### 2. Prompt Only

Run with dynamic intervals (agent chooses timing based on context):

```
/loop check the deploy status
/loop babysit the open PRs
```

### 3. Interval Only

Run the built-in maintenance prompt at a fixed interval:

```
/loop 15m
```

The maintenance prompt prioritizes:
1. Continue unfinished work from the conversation
2. Address PR comments and CI failures on the current branch
3. Run cleanup passes (bug hunts, simplification) when idle

## Interval Format

Supports the following suffixes:
- `s` - seconds (minimum: 60s, rounds up to 1 minute)
- `m` - minutes
- `h` - hours
- `d` - days

Examples: `30s`, `5m`, `2h`, `1d`

**Note**: Intervals that don't map cleanly to minutes are rounded.

## Behavior

- **Fixed intervals**: Task fires on a schedule
- **Dynamic intervals**: Agent chooses timing (1m-1h) based on context
- **Max duration**: Tasks expire after 7 days
- **Max tasks**: 50 concurrent tasks per session
- **Session-bound**: Stops when session closes

## Task Management

When the user asks about scheduled tasks:

- **List tasks**: "what loop tasks do I have?"
- **Cancel task**: "cancel the deploy check loop"
- **Modify interval**: "change the PR check to run every hour"

## Examples

**User**: "/loop 5m check if the build is passing"

**Agent**: "I'll schedule a loop to check the build status every 5 minutes."

[Creates scheduled task with ID, e.g., "a3b7c9d2"]

"Scheduled loop task `a3b7c9d2`. The agent will check the build status every 5 minutes."

[Each iteration reports]: "Build check: still running..."
[When done]: "Build completed successfully! The loop will continue checking."

**User**: "/loop review my PRs"

**Agent**: "I'll periodically review your open PRs and report on CI status,
review comments, and merge readiness."

[Schedules with dynamic timing based on PR activity]

## Differences from Similar Features

- **Loop vs. Fork**: Fork duplicates a session; loop schedules repetitive tasks
- **Loop vs. Run**: Run executes once; loop repeats on a schedule

## Technical Details

Tasks are managed by a session-scoped scheduler (`internal/loop`).
Each task has:
- Unique 8-character ID
- Session binding
- Configurable interval
- Automatic expiration (7 days)
- Graceful cancellation on session close
