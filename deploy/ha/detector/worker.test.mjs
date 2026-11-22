import assert from "node:assert/strict";
import { test } from "node:test";
import { confirmOutage } from "../confirm-outage.mjs";
import { checkServices, services } from "./health.mjs";
import worker, { OutageDetector } from "./worker.mjs";

const minute = 60_000;
const epoch = 60_000 * 500;

function fixture(t, token = "test-dispatch-credential") {
  let state;
  let pending = Promise.resolve();
  const context = {
    storage: {
      async get() { return structuredClone(state); },
      async put(_key, value) { state = structuredClone(value); },
    },
    blockConcurrencyWhile(callback) {
      const result = pending.then(callback);
      pending = result.catch(() => {});
      return result;
    },
  };
  const input = { cell: 503, api: 204, github: 204, requests: [], probes: 0 };
  t.mock.method(globalThis, "fetch", async (url, options) => {
    assert.equal(options.redirect, "manual");
    switch (url) {
      case services.cell.url:
        input.probes++;
        return new Response(null, { status: input.cell });
      case services.api.url:
        return new Response(null, { status: input.api });
      default:
        assert.equal(url, "https://api.github.com/repos/headmesh-dev/headmesh/dispatches");
        assert.equal(options.method, "POST");
        assert.equal(options.headers.Authorization, `Bearer ${token}`);
        input.requests.push(JSON.parse(options.body));
        return new Response(null, { status: input.github });
    }
  });
  const env = { GITHUB_DISPATCH_TOKEN: token };
  env.DETECTOR = {
    idFromName(name) { assert.equal(name, "headmesh"); return name; },
    get() {
      // Reinstantiate the object to make each tick depend on persisted state.
      const detector = new OutageDetector(context, env);
      return { fetch: (url, options) => detector.fetch(new Request(url, options)) };
    },
  };
  const tick = (offset) => worker.scheduled({ scheduledTime: epoch + offset * minute }, env);
  return { input, env, context, tick };
}

test("three consecutive failed slots dispatch once and health rearms the detector", async (t) => {
  const { input, tick } = fixture(t);
  await tick(0);
  await Promise.all([tick(1), tick(1)]);
  await tick(0);
  assert.equal(input.probes, 2);
  assert.equal(input.requests.length, 0);
  await tick(2);
  assert.deepEqual(input.requests, [{
    event_type: "control-plane-down",
    client_payload: { outage_id: new Date(epoch + 2 * minute).toISOString(), failed_services: ["cell"] },
  }]);
  await tick(3);
  assert.equal(input.requests.length, 1);
  input.cell = 200;
  await tick(4);
  input.api = 503;
  await tick(5);
  await tick(6);
  await tick(7);
  assert.equal(input.requests.length, 2);
  assert.deepEqual(input.requests[1].client_payload.failed_services, ["api"]);
});

test("missing cron slots and alternating failures do not accumulate toward a dispatch", async (t) => {
  const { input, tick } = fixture(t);
  await tick(0);
  await tick(1);
  await tick(3);
  assert.equal(input.requests.length, 0);
  input.cell = 200;
  input.api = 503;
  await tick(4);
  input.cell = 503;
  input.api = 204;
  await tick(5);
  input.cell = 200;
  input.api = 503;
  await tick(6);
  assert.equal(input.requests.length, 0);
});

test("a rejected dispatch retries with the same outage ID", async (t) => {
  const { input, tick } = fixture(t);
  await tick(0);
  await tick(1);
  input.github = 503;
  await assert.rejects(tick(2), /cron failed \(502\)/);
  input.github = 204;
  await tick(3);
  await tick(4);
  assert.equal(input.requests.length, 2);
  assert.deepEqual(input.requests[0], input.requests[1]);
});

test("the public status route is read-only and marks stale data unknown", async (t) => {
  const { input, tick, env, context } = fixture(t, "");
  const request = (path, method = "GET") => worker.fetch(new Request(`https://status.headmesh.dev${path}`, { method }), env);
  assert.equal((await request("/tick", "POST")).status, 404);
  assert.equal((await request("/status.json", "POST")).status, 405);
  assert.equal((await (await request("/status.json")).json()).status, "unknown");
  await tick(0);
  await tick(1);
  await tick(2);
  assert.equal(input.requests.length, 0);
  assert.deepEqual((await (await request("/status.json")).json()).components,
                   { cell: "unavailable", api: "operational" });
  assert.match(await (await request("/")).text(), /Mesh coordination/);
  assert.equal(await (await request("/", "HEAD")).text(), "");
  const state = await context.storage.get("state");
  state.checkedAt -= 6 * minute;
  await context.storage.put("state", state);
  assert.equal((await (await request("/status.json")).json()).status, "unknown");
});

test("health probes require the documented status and treat a network failure as down", async () => {
  const observed = await checkServices(async (url, options) => {
    assert.equal(options.cache, "no-store");
    assert.ok(options.signal instanceof AbortSignal);
    if (url === services.api.url) throw new Error("connection lost");
    return new Response(null, { status: 301 });
  });
  assert.deepEqual(observed, { cell: false, api: false });
});

test("independent confirmation requires the same service to fail all three probes", async () => {
  let attempts = 0;
  const pauses = [];
  const pause = async (delay) => { pauses.push(delay); };
  const confirmed = await confirmOutage(async () => {
    attempts++;
    return { cell: false, api: attempts === 2 };
  }, pause);
  assert.deepEqual(confirmed, { confirmed: true, failed_services: ["cell"] });
  assert.deepEqual(pauses, [minute, minute]);
  const probes = [{ cell: true, api: false }, { cell: false, api: true }];
  assert.equal((await confirmOutage(async () => probes.shift(), pause)).confirmed, false);
  assert.equal(probes.length, 0);
});
