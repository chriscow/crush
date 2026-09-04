Recover the exact original content of a tool result referenced by a context projection.

- Provide the exact `ref` shown in projected context, such as `t12`.
- `offset` and `limit` count Unicode code points, not bytes or lines.
- An omitted `limit` returns up to 12,000 code points; the maximum effective limit is 24,000.
- If `complete` is false, query the same ref again using `next_offset`, then concatenate each `content` page in order.
- `not_found` means the ref cannot be recovered in the current trusted session. Do not infer whether it existed in another session.

The session is supplied by trusted execution context and is intentionally not a parameter.
