import { describe, expect, it } from "vitest";
import { cspHash, extractInline, fillHeaders, MAX_HEADER_LINE } from "../../scripts/csp.mjs";

describe("cspHash", () => {
  it("matches the browser's hash of the exact script text", () => {
    // echo -n 'alert(1)' | openssl dgst -sha256 -binary | base64
    expect(cspHash("alert(1)")).toBe("'sha256-bhHHL3z2vDgxUt0W3dWQOrprscmda2Y5pLsLg4GF+pI='");
  });
});

describe("extractInline", () => {
  it("collects inline scripts and styles but not external scripts", () => {
    const html = `<head><script>a()</script><script type="module">b()</script><script src="/x.js"></script>
      <script defer src="https://static.cloudflareinsights.com/beacon.min.js" data-cf-beacon='{"token":"t"}'></script>
      <style>body{color:red}</style></head>`;
    const { scripts, styles, hasEventHandlers } = extractInline(html);
    expect(scripts).toEqual(["a()", "b()"]);
    expect(styles).toEqual(["body{color:red}"]);
    expect(hasEventHandlers).toBe(false);
  });

  it("flags inline event handlers", () => {
    expect(extractInline('<button onclick="go()">x</button>').hasEventHandlers).toBe(true);
    expect(extractInline('<a data-online="1">x</a>').hasEventHandlers).toBe(false);
  });
});

describe("fillHeaders", () => {
  const template =
    "/*\n  Content-Security-Policy: script-src 'self' {{SCRIPT_HASHES}}; style-src 'self' {{STYLE_HASHES}};\n";

  it("fills sorted, de-duplicated hashes and tidies empty slots", () => {
    const filled = fillHeaders(template, { scriptHashes: ["'sha256-b'", "'sha256-a'", "'sha256-b'"], styleHashes: [] });
    expect(filled).toContain("script-src 'self' 'sha256-a' 'sha256-b';");
    expect(filled).toContain("style-src 'self';");
  });

  it("refuses templates without placeholders and lines Pages would reject", () => {
    expect(() => fillHeaders("/*\n  X: y\n", { scriptHashes: [], styleHashes: [] })).toThrow(/missing/);
    const many = Array.from({ length: MAX_HEADER_LINE / 50 }, (_, i) => `'sha256-${String(i).padStart(44, "0")}'`);
    expect(() => fillHeaders(template, { scriptHashes: many, styleHashes: [] })).toThrow(/over 2000/);
  });
});
