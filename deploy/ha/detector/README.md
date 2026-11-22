# Headmesh outage detector

Cloudflare runs the detector once a minute. It checks the Headscale health
endpoint and the Headmesh API readiness endpoint in parallel. A SQLite-backed
Durable Object keeps the latest observation and a separate failure streak for
each service. Repeated or out-of-order cron slots do not advance a streak.
[Cloudflare Cron Triggers](https://developers.cloudflare.com/workers/configuration/cron-triggers/) · [Durable Object storage](https://developers.cloudflare.com/durable-objects/best-practices/access-durable-objects-storage/)

The configured custom domain serves a status page at `/` and JSON at
`/status.json`. The endpoints provide the last check time and the state of
both services. Status becomes unknown after five minutes without a check.

After three consecutive failures of a service, the detector sends a
`control-plane-down` repository dispatch to `headmesh-dev/headmesh` when
`GITHUB_DISPATCH_TOKEN` is configured. Acknowledged dispatches are sent once
per outage. A failed or interrupted dispatch can be retried with the same
outage ID while failures persist. Both services must pass a probe to rearm
the detector.

## Local checks

From the repository root, run the detector tests and build the Worker with
the pinned Wrangler release.

```sh
node --test deploy/ha/detector/worker.test.mjs
pnpm dlx wrangler@4.166.0 deploy --config deploy/ha/detector/wrangler.jsonc --dry-run
```

The recovery runner confirms an outage again from its own network before
acting on it. The command takes up to three samples one minute apart. Its
JSON reports which service failed every sample and it exits successfully
only when an outage is confirmed.

```sh
node deploy/ha/confirm-outage.mjs
```

## Deployment

The Worker can publish status without a GitHub token. Configure its
dispatch secret once the recovery workflow is ready to consume the event
and enforce the independent health check. For `repository_dispatch`,
GitHub requires the fine-grained **Contents: Read and write** permission.
Use a dedicated credential restricted to `headmesh-dev/headmesh`. Store
it as `GITHUB_DISPATCH_TOKEN` on the Worker.
[GitHub repository dispatch permissions](https://docs.github.com/en/rest/repos/repos#create-a-repository-dispatch-event) · [Cloudflare Worker secrets](https://developers.cloudflare.com/workers/configuration/secrets/)
