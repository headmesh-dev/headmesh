import { mkdtempSync, rmSync } from "node:fs";
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { createListener } from "../../server/app.ts";
import { loadConfig } from "../../server/config.ts";
import { migrate, openDatabase } from "../../server/db.ts";
import { WaitlistStore } from "../../server/store.ts";
import { IP, signupBody, stubUpstreams, testConfig, type Upstreams } from "./helpers.ts";

vi.mock(import("../../shared/site.ts"), async (importOriginal) => {
  const original = await importOriginal();
  return { ...original, SENDER: { ...original.SENDER, postalAddress: "1 Test Street, Toronto ON M5V 0A1, Canada" } };
});

describe("the HTTP service over a real socket, on a file database", () => {
  let dir: string;
  let server: Server;
  let base: string;
  let upstreams: Upstreams;

  beforeAll(async () => {
    dir = mkdtempSync(join(tmpdir(), "waitlist-"));
    const db = openDatabase(join(dir, "waitlist.db"));
    migrate(db);
    server = createServer(createListener({ config: testConfig, store: new WaitlistStore(db) }));
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  });

  afterAll(async () => {
    await new Promise((resolve) => server.close(resolve));
    rmSync(dir, { recursive: true, force: true });
  });

  beforeEach(() => {
    upstreams = stubUpstreams();
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("serves health and readiness", async () => {
    expect((await fetch(`${base}/healthz`)).status).toBe(204);
    expect((await fetch(`${base}/readyz`)).status).toBe(204);
  });

  it("takes a sign-up end to end, using the client address nginx passes", async () => {
    const response = await fetch(`${base}/api/waitlist`, {
      method: "POST",
      headers: { "content-type": "application/json", "x-real-ip": IP },
      body: JSON.stringify(signupBody()),
    });
    expect(response.status).toBe(202);
    expect(response.headers.get("content-type")).toBe("application/json; charset=utf-8");
    expect(response.headers.get("x-content-type-options")).toBe("nosniff");
    expect(upstreams.turnstileCalls[0]?.remoteip).toBe(IP);
    expect(upstreams.sent).toHaveLength(1);
  });

  it("refuses bodies over 8 KiB, even without a Content-Length", async () => {
    const big = new ReadableStream({
      start(controller) {
        controller.enqueue(new TextEncoder().encode(JSON.stringify(signupBody({ use_case: "x".repeat(20_000) }))));
        controller.close();
      },
    });
    const response = await fetch(`${base}/api/waitlist`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: big,
      duplex: "half",
    } as RequestInit);
    expect(response.status).toBe(413);
  });
});

describe("migrations", () => {
  it("apply once and are recorded", () => {
    const dir = mkdtempSync(join(tmpdir(), "waitlist-"));
    try {
      const path = join(dir, "waitlist.db");
      const first = openDatabase(path);
      expect(migrate(first)).toEqual(["0001_create_waitlist.sql"]);
      // A second process (the other colour) finds nothing to do.
      const second = openDatabase(path);
      expect(migrate(second)).toEqual([]);
      expect(second.prepare("PRAGMA journal_mode").get()).toEqual({ journal_mode: "wal" });
      first.close();
      second.close();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

describe("loadConfig", () => {
  const env = {
    WAITLIST_DB_PATH: "/var/lib/headmesh/prod/waitlist/waitlist.db",
    SITE_URL: "https://headmesh.dev/",
    EMAIL_FROM: "Headmesh <waitlist@mail.headmesh.dev>",
    TURNSTILE_SECRET_KEY: "secret",
    RESEND_API_KEY: "re_key",
    WAITLIST_SECRET: "x".repeat(48),
  };

  it("applies defaults and normalises the site URL", () => {
    expect(loadConfig(env)).toMatchObject({
      host: "127.0.0.1",
      port: 8787,
      siteUrl: "https://headmesh.dev",
      emailDelivery: "resend",
      emailReplyTo: undefined,
      clientIpHeader: "x-real-ip",
    });
  });

  it("refuses to start with missing or weak settings", () => {
    expect(() => loadConfig({ ...env, WAITLIST_SECRET: "short" })).toThrow(/at least 32/);
    expect(() => loadConfig({ ...env, RESEND_API_KEY: "" })).toThrow(/RESEND_API_KEY is required/);
    expect(() => loadConfig({ ...env, SITE_URL: "ftp://headmesh.dev" })).toThrow(/SITE_URL/);
    expect(() => loadConfig({ ...env, WAITLIST_PORT: "99999" })).toThrow(/WAITLIST_PORT/);
    expect(loadConfig({ ...env, RESEND_API_KEY: "", EMAIL_DELIVERY: "log" }).emailDelivery).toBe("log");
  });
});
