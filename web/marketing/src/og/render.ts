// Build-time Open Graph images (1200×630 PNG), rendered with Satori and resvg. Nothing here runs in the browser.

import interRegular from "@fontsource/inter/files/inter-latin-400-normal.woff?inline";
import interBold from "@fontsource/inter/files/inter-latin-700-normal.woff?inline";
import { Resvg } from "@resvg/resvg-js";
import satori from "satori";

const WIDTH = 1200;
const HEIGHT = 630;

const COLORS = { bg: "#0a0e13", fg: "#e8eef4", muted: "#9aa8b6", accent: "#3fd8c2", line: "#232d38" };

const MARK_SVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" fill="none"><g stroke="${COLORS.accent}" stroke-width="2.25" stroke-linecap="round" stroke-linejoin="round"><path d="M7 8h18L16 25z"/><path d="M7 8l9 7 9-7M16 15v10"/></g><g fill="${COLORS.accent}"><circle cx="7" cy="8" r="3.25"/><circle cx="25" cy="8" r="3.25"/><circle cx="16" cy="25" r="3.25"/><circle cx="16" cy="15" r="2.5"/></g></svg>`;

function fontData(dataUrl: string): ArrayBuffer {
  const bytes = Buffer.from(dataUrl.slice(dataUrl.indexOf(",") + 1), "base64");
  return bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength) as ArrayBuffer;
}

const fonts = [
  { name: "Inter", data: fontData(interRegular), weight: 400 as const, style: "normal" as const },
  { name: "Inter", data: fontData(interBold), weight: 700 as const, style: "normal" as const },
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
      backgroundImage: "radial-gradient(900px 420px at 85% -10%, #0f2a28, transparent)",
      color: COLORS.fg,
      fontFamily: "Inter",
    },
    [
      el("div", { display: "flex", alignItems: "center", gap: "18px" }, [
        el("img", { width: 56, height: 56 }, undefined, {
          src: `data:image/svg+xml;base64,${Buffer.from(MARK_SVG).toString("base64")}`,
          width: 56,
          height: 56,
        }),
        el("div", { fontSize: 40, fontWeight: 700, letterSpacing: "-0.02em" }, "Headmesh"),
      ]),
      el("div", { display: "flex", flexDirection: "column", gap: "24px" }, [
        el("div", { fontSize: 72, fontWeight: 700, lineHeight: 1.08, letterSpacing: "-0.03em", maxWidth: 1000 }, title),
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

/** Square app icon for iOS home screens: the mark on a full-bleed background (iOS rounds the corners). */
export function renderTouchIcon(size = 180): Uint8Array<ArrayBuffer> {
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="${size}" height="${size}"><rect width="32" height="32" fill="${COLORS.bg}"/><g transform="translate(4.8 4.8) scale(0.7)">${MARK_SVG.replace(/^<svg[^>]*>|<\/svg>$/g, "")}</g></svg>`;
  return new Uint8Array(new Resvg(svg, { fitTo: { mode: "width", value: size } }).render().asPng());
}
