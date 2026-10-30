// Build-time Open Graph images (1200×630 PNG), rendered with Satori and resvg. Nothing here runs in the browser.

import bricolageExtraBold from "@fontsource/bricolage-grotesque/files/bricolage-grotesque-latin-800-normal.woff?inline";
import geistRegular from "@fontsource/geist/files/geist-latin-400-normal.woff?inline";
import geistBold from "@fontsource/geist/files/geist-latin-700-normal.woff?inline";
import { Resvg } from "@resvg/resvg-js";
import satori from "satori";

const WIDTH = 1200;
const HEIGHT = 630;

const COLORS = { bg: "#070B12", fg: "#F2F6F9", muted: "#9AA8BC", accent: "#3CEFB4", line: "#1C2736" };

const MARK_SVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="76 76 360 360"><defs><linearGradient id="shared-leg" gradientUnits="userSpaceOnUse" x1="222" y1="280" x2="290" y2="424"><stop offset="0" stop-color="#3CEFB4"/><stop offset="1" stop-color="#27BFF5"/></linearGradient></defs><g fill="#F2F6F9"><path d="M88 390V122a34 34 0 0 1 68 0v268a34 34 0 0 1-68 0Z"/><path d="M356 390V122a34 34 0 0 1 68 0v268a34 34 0 0 1-68 0Z"/><path d="M88 280a101 101 0 0 1 202 0h-68a33 33 0 0 0-66 0Z"/><path d="M222 280a101 101 0 0 1 202 0h-68a33 33 0 0 0-66 0Z"/></g><path fill="url(#shared-leg)" d="M222 280h68v110a34 34 0 0 1-68 0Z"/></svg>`;

function fontData(dataUrl: string): ArrayBuffer {
  const bytes = Buffer.from(dataUrl.slice(dataUrl.indexOf(",") + 1), "base64");
  return bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength) as ArrayBuffer;
}

const fonts = [
  { name: "Geist", data: fontData(geistRegular), weight: 400 as const, style: "normal" as const },
  { name: "Geist", data: fontData(geistBold), weight: 700 as const, style: "normal" as const },
  {
    name: "Bricolage Grotesque",
    data: fontData(bricolageExtraBold),
    weight: 800 as const,
    style: "normal" as const,
  },
];

type Node = {
  type: string;
  props: Record<string, unknown> & { children?: Node | string | (Node | string)[] | undefined };
};

const el = (type: string, style: Record<string, unknown>, children?: Node["props"]["children"], extra = {}): Node => ({
  type,
  props: { style, children, ...extra },
});

export type OgCard = {
  title: string;
  subtitle: string;
};

export async function renderOgImage({ title, subtitle }: OgCard): Promise<Uint8Array<ArrayBuffer>> {
  const tree = el(
    "div",
    {
      width: "100%",
      height: "100%",
      display: "flex",
      flexDirection: "column",
      justifyContent: "space-between",
      padding: "72px 80px",
      backgroundColor: COLORS.bg,
      backgroundImage: "radial-gradient(900px 420px at 85% -10%, #0f2e2c, transparent)",
      color: COLORS.fg,
      fontFamily: "Geist",
    },
    [
      el("div", { display: "flex", alignItems: "center", gap: "18px" }, [
        el("img", { width: 56, height: 56 }, undefined, {
          src: `data:image/svg+xml;base64,${Buffer.from(MARK_SVG).toString("base64")}`,
          width: 56,
          height: 56,
        }),
        el(
          "div",
          { fontFamily: "Bricolage Grotesque", fontSize: 40, fontWeight: 800, letterSpacing: "-0.025em" },
          "headmesh",
        ),
      ]),
      el("div", { display: "flex", flexDirection: "column", gap: "24px" }, [
        el(
          "div",
          {
            fontFamily: "Bricolage Grotesque",
            fontSize: 72,
            fontWeight: 800,
            lineHeight: 1.08,
            letterSpacing: "-0.03em",
            maxWidth: 1000,
          },
          title,
        ),
        el("div", { fontSize: 32, lineHeight: 1.35, color: COLORS.muted, maxWidth: 960 }, subtitle),
      ]),
      el(
        "div",
        {
          display: "flex",
          justifyContent: "space-between",
          borderTop: `2px solid ${COLORS.line}`,
          paddingTop: "28px",
          fontSize: 26,
          color: COLORS.muted,
        },
        [
          el("div", { color: COLORS.accent, fontWeight: 700 }, "headmesh.dev"),
          el("div", {}, "Open source · Self-hostable · Multi-tenant"),
        ],
      ),
    ],
  );

  // Satori's types expect a ReactNode; this plain object tree is the documented React-free form.
  const svg = await satori(tree as unknown as Parameters<typeof satori>[0], { width: WIDTH, height: HEIGHT, fonts });
  return new Uint8Array(new Resvg(svg, { fitTo: { mode: "width", value: WIDTH } }).render().asPng());
}

/**
 * Square app icon for iOS home screens: the mark on a full-bleed background (iOS rounds the corners), inset on the
 * brand kit's 512 canvas so the rounding never clips the legs.
 */
export function renderTouchIcon(size = 180): Uint8Array<ArrayBuffer> {
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512" width="${size}" height="${size}"><rect width="512" height="512" fill="${COLORS.bg}"/>${MARK_SVG.replace(/^<svg[^>]*>|<\/svg>$/g, "")}</svg>`;
  return new Uint8Array(new Resvg(svg, { fitTo: { mode: "width", value: size } }).render().asPng());
}
