Launch a new agent that has access to the following tools: glob, grep, ls, view. When you are searching for a keyword or file and are not confident that you will find the right match on the first try, use the agent tool to perform the search for you.

Set subagent_type to "general-purpose" to grant full tools including edit, write, bash. Omit or set to empty for read-only task agent. Set model to "small" for fast, low-cost lookups and code exploration, or "large" for complex work. When omitted, the configured subagent model is used, which defaults to "large".
