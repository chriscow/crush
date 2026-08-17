---
name: fork
description:
  Fork the current session to create an independent copy with full conversation
  history. Use when the user asks to fork, branch, or duplicate a conversation,
  or when they want to explore alternative approaches without affecting the current
  session.
user-invocable: true
---

# Fork Session

Fork creates an independent copy of the current session with its complete message
history. The forked session gets a new ID but contains all messages from the original
session, allowing both to evolve independently.

## Usage

When the user asks to fork:

1. Call the `crush_info` tool to get the current session ID
2. Use the session service to create a fork
3. Inform the user about the new session

## Behavior

- The original session remains unchanged
- The forked session has a new UUID
- All messages are copied to the new session
- The title gets " (fork)" appended
- Both sessions can continue independently

## Examples

**User**: "fork this session"

**Agent**: "I'll fork the current session to create an independent copy."

[Uses crush_info to get current session]
[Calls session service Fork method]

"I've created a forked session with ID `{new-session-id}`. The original session
remains unchanged, and both can now evolve independently."

## Use Cases

- **Testing approaches**: Try different solutions without losing progress
- **Parallel exploration**: Investigate multiple directions simultaneously
- **Risk mitigation**: Make experimental changes while keeping a stable baseline
- **Collaboration**: Share a conversation snapshot while continuing your own work

## Differences from Similar Features

- **Fork vs. Resume**: Resume continues an existing session; fork creates a copy
- **Fork vs. New Session**: New session starts empty; fork copies the history
