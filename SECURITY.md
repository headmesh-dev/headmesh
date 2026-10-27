# Security policy

Headmesh controls who can reach which machines, so isolation bugs are treated as critical.

## Reporting a vulnerability

Please use GitHub's private vulnerability reporting on `headmesh-dev/headmesh`
("Security" → "Report a vulnerability"), or email **security@headmesh.dev**.
Do not open public issues for security problems.

We aim to acknowledge reports within 2 business days and to ship a fix or mitigation for critical issues
(cross-tenant reachability, authentication bypass, key disclosure) within 7 days.

## Scope

In scope: the control server, cell agent, CLI, sandbox daemon and image, plugins, and the hosted service at
`*.headmesh.dev`. Out of scope: vulnerabilities in Headscale or Tailscale themselves (report those upstream; we
will help coordinate), and denial of service through volumetric traffic.

## Guarantees we test for

- A device in one organisation can never reach a device in another unless an active, time-limited grant allows
  exactly that device and port.
- Revoking a grant or removing a device takes effect within seconds and removes keys and routes.
- Every enrollment, grant and revocation produces a tamper-evident audit entry.
