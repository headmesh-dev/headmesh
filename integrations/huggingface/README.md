---
title: Xcelsior and Headmesh MCP
emoji: 🔗
colorFrom: indigo
colorTo: blue
sdk: static
app_file: index.html
pinned: false
license: apache-2.0
tags:
  - mcp
  - developer-tools
  - oauth
  - gpu
short_description: GPU compute and isolated development sandboxes through MCP.
---

# Xcelsior and Headmesh MCP

Public connection documentation for two Model Context Protocol servers:

- **Xcelsior:** GPU compute discovery and management at
  `https://mcp.xcelsior.ca/mcp`, with scoped OAuth for authenticated actions.
- **Headmesh:** files, commands, Git and persistent sessions in your own isolated
  sandbox, using `https://YOUR_SANDBOX_HOST/mcp` and an explicit access grant.

This static documentation Space makes no inference calls and hosts no sandbox,
models, shared credentials or MCP endpoint. Connect your assistant directly to
the authenticated service. Publication here does not confer access or establish
acceptance into another provider's curated directory.

Headmesh supports GitHub and Hugging Face browser sign-in, explicit consent,
PKCE S256, CIMD/DCR clients, scoped access and rotating refresh tokens.
Existing per-client sandbox bearer credentials are also supported.

Links:

- https://xcelsior.ca
- https://github.com/aabiro/xcelsior
- https://headmesh.dev
- https://github.com/headmesh-dev/headmesh
- https://github.com/headmesh-dev/headmesh/blob/main/docs/mcp-oauth.md

The source for this documentation is maintained under
`integrations/huggingface/` in the Headmesh repository.
