# Connect to HeurChain with MCP

Choose **one** deployment. Hosted and self-hosted credentials and stores are separate.

## Hosted service

The [official registry entry for `com.heurchain/heurchain` v0.6.1](https://registry.modelcontextprotocol.io/v0.1/servers/com.heurchain%2Fheurchain/versions/0.6.1) describes a remote **Streamable HTTP** server. [server.json](server.json) mirrors its public server descriptor without registry-generated status metadata. It is not a client config or a command to run.

In an MCP client that supports remote HTTP and custom headers, configure:

| Field | Value |
|---|---|
| Transport | Streamable HTTP (despite the URL ending in `/sse`) |
| URL | `https://api.heurchain.com/mcp/sse` |
| Required header: `X-HeurChain-API-Key` | Your hosted API key, provided through the client's secret store |
| Required header: `X-HeurChain-Tenant` | Your hosted tenant ID |

Use the values for the intended workspace from your HeurChain dashboard. Do not commit credentials. Once connected, use the client's tool discovery to see what your deployment exposes; no memory writes are needed to check the connection.

## Self-hosted broker

Follow the repository's [Docker Compose quick start](../README.md#quick-start) to run the broker and create a local tenant/key. For a default local installation, select Streamable HTTP and `http://127.0.0.1:8080/mcp/sse` from a client on the same machine. Use the **local** key and tenant, never the hosted values. For Claude Code and Codex CLI, [`tools/setup`](../docs/agent-setup.md) adds the entry, the skill and the optional automatic recall hook in one local flow. See the [installation guide](../docs/install.md) for other deployment and client settings.

The self-hosted broker release is `0.6.1-oss.1`; the [`heurchain` npm package](../npm/README.md) is separately versioned and bundles the broker source, its Compose stack and a `heurchain` CLI that installs and runs them (`npx heurchain init`, `up`, `tenant`, `connect`). For instructions on using memory after connecting, see the [agent skill](../skills/heurchain-memory/SKILL.md) or its [canonical Markdown download](https://heurchain.com/skills/heurchain-memory/SKILL.md).
