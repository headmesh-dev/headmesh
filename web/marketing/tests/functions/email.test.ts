import { describe, expect, it } from "vitest";
import { normalizeEmail } from "../../server/email";

describe("normalizeEmail", () => {
  it.each([
    ["  Ada@Example.COM ", "ada@example.com"],
    ["ada@example.com.", "ada@example.com"],
    ["first.last+waitlist@gmail.com", "first.last+waitlist@gmail.com"],
    ["ada@bücher.example", "ada@xn--bcher-kva.example"],
    ["ADA@XN--BCHER-KVA.EXAMPLE", "ada@xn--bcher-kva.example"],
    ['"quoted@local"@example.com', '"quoted@local"@example.com'],
  ])("normalises %j to %j", (input, expected) => {
    expect(normalizeEmail(input)).toBe(expected);
  });

  it.each(["", "   ", "no-at-sign", "@example.com", "ada@", "ada@exa mple.com", "ada@example.com/path", "ada@[::1"])(
    "rejects %j",
    (input) => {
      expect(normalizeEmail(input)).toBeNull();
    },
  );

  it("treats NFC and NFD spellings as the same address", () => {
    const nfd = "réne@example.com";
    const nfc = "réne@example.com";
    expect(normalizeEmail(nfd)).toBe(normalizeEmail(nfc));
  });
});
