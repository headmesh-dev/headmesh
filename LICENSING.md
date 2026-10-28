# Licensing

Headmesh is open source under two licenses (ADR 0015). Copyright © 2026 Xcelsior Compute Inc.

| Path | License | Why |
| --- | --- | --- |
| Everything not listed below: `cli/`, `cmd/headmesh/` (the CLI), `sandbox/`, `plugins/`, `proto/cell/`, `gen/cell/`, `web/`, `deploy/`, `docs/` | [Apache-2.0](LICENSE) | Things people embed, extend or build clients on: use them anywhere, including in proprietary products |
| `api/`, `core/`, `internal/`, `db/`, `cmd/headmesh-server/`, `cmd/headmesh-cellagent/`, `test/` | [AGPL-3.0-only](api/LICENSE) (a copy in each directory) | The control plane. Anyone may run, modify and host it, but a modified version offered as a network service must publish its source |
| `proto/headscale/`, `gen/headscale/` | [BSD-3-Clause](proto/headscale/LICENSE), from [Headscale](https://github.com/juanfont/headscale) | Vendored Headscale v0.29.6 API definitions and code generated from them |

The Apache-2.0 parts never import the AGPL parts: the CLI talks to the server only over its REST API, and the
sandbox is a separate Go module. A test in CI could enforce this later; for now `go list -deps ./cmd/headmesh`
shows it.

The Headmesh name and logo (including `web/marketing/public/brand/` and `web/marketing/src/assets/`) are trademarks of
Xcelsior Compute Inc. and are not covered by these licenses.

Contributions are accepted under the license of the directory they change. Xcelsior Compute Inc. may later offer
future versions under different terms; versions already released stay available under the licenses they were
released with.
