import { describe, expect, it } from "vitest";
import { CONFIRM_TOKEN_TTL_SECONDS, hashIp, signToken, verifyToken } from "../../server/crypto";

const SECRET = "unit-test-secret-with-at-least-32-characters";
const ID = "4f8e2a8c-3b2d-4c1e-9a7f-0123456789ab";
const NOW = 1_800_000_000;

describe("signed tokens", () => {
  it("round-trips a confirm token and never contains the email", async () => {
    const token = await signToken(SECRET, "confirm", ID, NOW);
    expect(token).toMatch(/^v1\.[0-9a-f-]{36}\.\d+\.[A-Za-z0-9_-]+$/);
    expect(await verifyToken(SECRET, "confirm", token, NOW)).toBe(ID);
  });

  it("expires confirm tokens after the TTL", async () => {
    const token = await signToken(SECRET, "confirm", ID, NOW);
    expect(await verifyToken(SECRET, "confirm", token, NOW + CONFIRM_TOKEN_TTL_SECONDS)).toBe(ID);
    expect(await verifyToken(SECRET, "confirm", token, NOW + CONFIRM_TOKEN_TTL_SECONDS + 1)).toBeNull();
  });

  it("never expires unsubscribe tokens", async () => {
    const token = await signToken(SECRET, "unsubscribe", ID, NOW);
    expect(await verifyToken(SECRET, "unsubscribe", token, NOW + 10 * 365 * 86_400)).toBe(ID);
  });

  it("keeps purposes apart", async () => {
    const unsubscribe = await signToken(SECRET, "unsubscribe", ID, NOW);
    const confirm = await signToken(SECRET, "confirm", ID, NOW);
    expect(await verifyToken(SECRET, "confirm", unsubscribe, NOW)).toBeNull();
    expect(await verifyToken(SECRET, "unsubscribe", confirm, NOW)).toBeNull();
    // Same shape, wrong key: forge a 3-part token from the confirm signature.
    const [v, id, , sig] = confirm.split(".");
    expect(await verifyToken(SECRET, "unsubscribe", `${v}.${id}.${sig}`, NOW)).toBeNull();
  });

  it("rejects tampering, other secrets and malformed input", async () => {
    const token = await signToken(SECRET, "confirm", ID, NOW);
    const parts = token.split(".");
    const otherId = parts.with(1, "00000000-0000-4000-8000-000000000000").join(".");
    const longerExpiry = parts.with(2, String(Number(parts[2]) + 1)).join(".");
    for (const forged of [otherId, longerExpiry]) {
      expect(await verifyToken(SECRET, "confirm", forged, NOW)).toBeNull();
    }
    expect(await verifyToken(`${SECRET}-rotated`, "confirm", token, NOW)).toBeNull();
    for (const junk of ["", "v1", "v2.x.y.z", `${token}.extra`, "v1.not-a-uuid.1.abc", `${token}!`, "a".repeat(300)]) {
      expect(await verifyToken(SECRET, "confirm", junk, NOW)).toBeNull();
    }
  });

  it("refuses short secrets", async () => {
    await expect(signToken("too-short", "confirm", ID, NOW)).rejects.toThrow(/at least 32/);
  });
});

describe("hashIp", () => {
  it("is stable per secret and doesn't reveal the address", async () => {
    const a = await hashIp(SECRET, "203.0.113.7");
    expect(a).toBe(await hashIp(SECRET, "203.0.113.7"));
    expect(a).not.toBe(await hashIp(SECRET, "203.0.113.8"));
    expect(a).not.toBe(await hashIp(`${SECRET}-other`, "203.0.113.7"));
    expect(a).not.toContain("203");
    expect(a).toHaveLength(32);
  });
});
