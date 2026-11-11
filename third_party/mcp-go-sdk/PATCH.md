# chagent fork: MCP protocol 2026-06-30 enabled

This directory is a minimal fork of `github.com/modelcontextprotocol/go-sdk v1.6.1`
(MIT, see LICENSE), used through a `replace` directive in the repository `go.mod`.

## Patch

Upstream v1.6.1 already contains:

- `protocolVersion20260630 = "2026-06-30"` constant;
- standard Streamable-HTTP header logic gated on `>= 2026-06-30`
  (`mcp/streamable_headers.go`, SEP-2575);
- feature/version handling that treats a higher version as backwards compatible.

But `latestProtocolVersion` was still `2025-11-25` and `2026-06-30` was excluded
from `supportedProtocolVersions`, so a client could neither request nor accept a
2026-06-30 server without downgrading. This fork changes only `mcp/shared.go`:

- `latestProtocolVersion = protocolVersion20260630`;
- `protocolVersion20260630` added first to `supportedProtocolVersions`.

`2025-11-25`, `2025-06-18`, `2025-03-26`, `2024-11-05` remain supported for
downgrade negotiation.

## Maintainership

Re-apply this patch when upgrading the fork/upstream. Keep changes minimal and
record them here. Do not add unrelated modifications; anything else must be
proposed upstream.
