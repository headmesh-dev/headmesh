import { checkServices, services } from "./health.mjs";

const minute = 60_000;
const serviceNames = Object.keys(services);
const dispatchURL = "https://api.github.com/repos/headmesh-dev/headmesh/dispatches";
const headers = { "Cache-Control": "no-store" };

async function dispatch(token, state, failures) {
  try {
    const response = await fetch(dispatchURL, {
      method: "POST",
      redirect: "manual",
      signal: AbortSignal.timeout(8000),
      headers: {
        "Accept": "application/vnd.github+json",
        "Authorization": `Bearer ${token}`,
        "Content-Type": "application/json",
        "User-Agent": "Headmesh-Recovery-Detector",
        "X-GitHub-Api-Version": "2026-03-10",
      },
      body: JSON.stringify({
        event_type: "control-plane-down",
        client_payload: { outage_id: state.episode.id, failed_services: failures },
      }),
    });
    await response.body?.cancel();
    return response.status === 204;
  } catch {
    return false;
  }
}

function publicStatus(state, now = Date.now()) {
  const fresh = state && now - state.checkedAt < 5 * minute;
  const components = Object.fromEntries(serviceNames.map((name) => [
    name, fresh ? (state.healthy[name] ? "operational" : "unavailable") : "unknown",
  ]));
  let status = "unknown";
  if (fresh) {
    const failures = serviceNames.filter((name) => !state.healthy[name]).length;
    status = ["operational", "degraded", "outage"][failures];
  }
  return { status, components, last_checked_at: state ? new Date(state.checkedAt).toISOString() : null };
}

export class OutageDetector {
  constructor(ctx, env) {
    this.ctx = ctx;
    this.env = env;
  }

  async fetch(request) {
    const path = new URL(request.url).pathname;
    if (request.method === "GET" && path === "/snapshot") {
      return Response.json(publicStatus(await this.ctx.storage.get("state")), { headers });
    }
    if (request.method !== "POST" || path !== "/tick") {
      return new Response(null, { status: 404 });
    }
    const scheduledTime = Number(await request.text());
    if (!Number.isSafeInteger(scheduledTime) || scheduledTime <= 0) {
      return new Response(null, { status: 400 });
    }
    const slot = Math.floor(scheduledTime / minute);
    return this.ctx.blockConcurrencyWhile(async () => {
      try {
        const sent = await this.tick(slot);
        return new Response(null, { status: sent ? 204 : 502 });
      } catch {
        return new Response(null, { status: 500 });
      }
    });
  }

  async tick(slot) {
    const previous = await this.ctx.storage.get("state");
    if (previous && slot <= previous.slot) return true;
    const healthy = await checkServices();
    const adjacent = previous && slot === previous.slot + 1;
    const counts = Object.fromEntries(serviceNames.map((name) => [
      name, healthy[name] ? 0 : (adjacent ? previous.counts[name] : 0) + 1,
    ]));
    const failures = serviceNames.filter((name) => counts[name] >= 3);
    const state = { slot, healthy, counts, checkedAt: Date.now(), episode: previous?.episode ?? null };
    if (serviceNames.every((name) => healthy[name])) {
      state.episode = null;
    } else if (failures.length && !state.episode) {
      state.episode = { id: new Date(slot * minute).toISOString(), dispatched: false };
    }
    await this.ctx.storage.put("state", state);
    if (!this.env.GITHUB_DISPATCH_TOKEN || !failures.length || state.episode.dispatched) return true;
    if (!await dispatch(this.env.GITHUB_DISPATCH_TOKEN, state, failures)) return false;
    state.episode.dispatched = true;
    await this.ctx.storage.put("state", state);
    return true;
  }
}

function detector(env) {
  return env.DETECTOR.get(env.DETECTOR.idFromName("headmesh"));
}

function statusPage(snapshot) {
  return `<!doctype html>
<html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Headmesh status</title><style>body{font:1rem system-ui;max-width:36rem;margin:4rem auto;padding:0 1.25rem;color-scheme:light dark}dt{font-weight:600}dd{margin:0 0 1rem}footer{margin-top:2rem}</style>
<main><h1>Headmesh status</h1><p>Overall status: ${snapshot.status}</p>
<dl><dt>Mesh coordination</dt><dd>${snapshot.components.cell}</dd><dt>Headmesh API</dt><dd>${snapshot.components.api}</dd></dl>
<footer>Last checked: ${snapshot.last_checked_at ?? "Awaiting the first check"} · <a href="/status.json">JSON status</a></footer></main></html>`;
}

export default {
  async scheduled(controller, env) {
    const result = await detector(env).fetch("https://detector.internal/tick", {
      method: "POST", body: String(controller.scheduledTime),
    });
    if (!result.ok) throw new Error(`Headmesh detector cron failed (${result.status})`);
  },

  async fetch(request, env) {
    const path = new URL(request.url).pathname;
    if (path !== "/" && path !== "/status.json") return new Response(null, { status: 404 });
    if (request.method !== "GET" && request.method !== "HEAD") {
      return new Response(null, { status: 405, headers: { Allow: "GET, HEAD" } });
    }
    const result = await detector(env).fetch("https://detector.internal/snapshot");
    if (!result.ok) return new Response(null, { status: 503, headers });
    const snapshot = await result.json();
    if (path === "/status.json") {
      const response = Response.json(snapshot, { headers });
      return request.method === "HEAD" ? new Response(null, { headers: response.headers }) : response;
    }
    return new Response(request.method === "HEAD" ? null : statusPage(snapshot), {
      headers: {
        ...headers,
        "Content-Type": "text/html; charset=utf-8",
        "Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'",
      },
    });
  },
};
