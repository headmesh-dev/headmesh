import { readFileSync } from "node:fs";
import { Resvg } from "@resvg/resvg-js";
import { describe, expect, it } from "vitest";

const publicRoot = new URL("../../public/", import.meta.url);
const kitRoot = new URL("brand/headmesh-v1/", publicRoot);
const read = (path: string, root = publicRoot) => readFileSync(new URL(path, root));

function pngImage(png: Buffer, size: number) {
  return new Resvg(
    `<svg xmlns="http://www.w3.org/2000/svg" width="${size}" height="${size}"><image width="${size}" height="${size}" href="data:image/png;base64,${png.toString("base64")}"/></svg>`,
    { font: { loadSystemFonts: false } },
  ).render();
}

function alpha(image: ReturnType<Resvg["render"]>, x: number, y: number) {
  return image.pixels[(Math.floor(y) * image.width + Math.floor(x)) * 4 + 3];
}

type Point = [number, number];
type Transform = (x: number, y: number) => Point;

function expectClearGaps(image: ReturnType<Resvg["render"]>, point: Transform) {
  for (const [x, y] of [
    [189, 340],
    [323, 340],
    [256, 150],
  ] as const) {
    const [px, py] = point(x, y);
    expect(alpha(image, px, py), `gap at ${x},${y}`).toBe(0);
  }
  for (const [x, y] of [
    [122, 340],
    [256, 340],
    [390, 340],
  ] as const) {
    const [px, py] = point(x, y);
    expect(alpha(image, px, py), `leg at ${x},${y}`).toBe(255);
  }
}

describe("transparent favicons", () => {
  it.each(["favicon.svg", "favicon-dark.svg"])("keeps %s identical to the brand kit with clear gaps", (filename) => {
    const svg = read(filename);
    expect(svg.equals(read(`icons/${filename}`, kitRoot))).toBe(true);
    const image = new Resvg(svg, { font: { loadSystemFonts: false }, fitTo: { mode: "width", value: 512 } }).render();
    expectClearGaps(image, (x, y) => [x, y]);
  });

  it("preserves the gaps in every PNG and ICO frame", () => {
    const ico = read("favicon.ico");
    expect(ico.equals(read("icons/favicon.ico", kitRoot))).toBe(true);
    expect(ico.readUInt16LE(4)).toBe(3);
    for (const [index, size] of [16, 32, 48].entries()) {
      const entry = 6 + index * 16;
      expect(ico[entry]).toBe(size);
      expect(ico[entry + 1]).toBe(size);
      const length = ico.readUInt32LE(entry + 8);
      const offset = ico.readUInt32LE(entry + 12);
      const png = read(`icons/favicon-${size}.png`, kitRoot);
      expect(ico.subarray(offset, offset + length).equals(png)).toBe(true);
      expectClearGaps(pngImage(png, size), (x, y) => [(x * size) / 512, (y * size) / 512]);
    }
  });
});

type Logo = {
  name: string;
  layout: string;
  style: string;
  background: string;
  svg: string;
  png: string;
  width: number;
};
const logos = JSON.parse(read("logos/manifest.json", kitRoot).toString()).assets as Logo[];

describe("transparent logo exports", () => {
  for (const logo of logos.filter((asset) => asset.background === "transparent" && asset.layout !== "wordmark")) {
    it(`${logo.name} leaves the arches and legs open in SVG and PNG`, () => {
      let point: Transform;
      if (logo.layout === "mark") {
        point =
          logo.style === "on-dark" || logo.style === "on-light"
            ? (x, y) => [((x - 76) * 512) / 360, ((y - 76) * 512) / 360]
            : (x, y) => [x, y];
      } else if (logo.layout === "horizontal") {
        point = (x, y) => [64 + ((x - 88) * 160) / 336, 60 + ((y - 88) * 160) / 336];
      } else {
        point = (x, y) => [220 + ((x - 88) * 280) / 336, 64 + ((y - 88) * 280) / 336];
      }
      const svg = new Resvg(read(`logos/${logo.svg}`, kitRoot), { font: { loadSystemFonts: false } }).render();
      expectClearGaps(svg, point);
      const png = read(`logos/${logo.png}`, kitRoot);
      const height = svg.height;
      const image = new Resvg(
        `<svg xmlns="http://www.w3.org/2000/svg" width="${logo.width}" height="${height}"><image width="${logo.width}" height="${height}" href="data:image/png;base64,${png.toString("base64")}"/></svg>`,
        { font: { loadSystemFonts: false } },
      ).render();
      expectClearGaps(image, point);
    });
  }
});
