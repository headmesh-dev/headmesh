import { copyFile, mkdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { Resvg } from "@resvg/resvg-js";
import satori from "satori";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(scriptDir, "../public/brand/headmesh-v1");
const referenceRoot = path.resolve(scriptDir, "../brand-reference/headmesh-process");
const wordmarkSource = await readFile(path.resolve(scriptDir, "../src/components/Wordmark.astro"), "utf8");
const wordmarkPath = wordmarkSource.match(/<path fill="currentColor" d="([^"]+)"/)?.[1];
if (!wordmarkPath) throw new Error("Could not read the outlined Bricolage Grotesque wordmark from Wordmark.astro");
const wordmarkBounds = wordmarkSource
  .match(/viewBox="([^"]+)"/)?.[1]
  .split(/\s+/)
  .map(Number);
if (wordmarkBounds?.length !== 4 || wordmarkBounds.some((value) => !Number.isFinite(value))) {
  throw new Error("Could not read the outlined wordmark bounds");
}
const fontRoot = path.resolve(scriptDir, "../node_modules");
const toArrayBuffer = (buffer) => buffer.buffer.slice(buffer.byteOffset, buffer.byteOffset + buffer.byteLength);
const [geistRegular, bricolageExtraBold, geistMonoRegular] = await Promise.all([
  readFile(path.join(fontRoot, "@fontsource/geist/files/geist-latin-400-normal.woff")),
  readFile(path.join(fontRoot, "@fontsource/bricolage-grotesque/files/bricolage-grotesque-latin-800-normal.woff")),
  readFile(path.join(fontRoot, "@fontsource/geist-mono/files/geist-mono-latin-400-normal.woff")),
]);
const socialFonts = [
  { name: "Geist", data: toArrayBuffer(geistRegular), weight: 400, style: "normal" },
  { name: "Bricolage Grotesque", data: toArrayBuffer(bricolageExtraBold), weight: 800, style: "normal" },
  { name: "Geist Mono", data: toArrayBuffer(geistMonoRegular), weight: 400, style: "normal" },
];
const colors = {
  ink: "#070B12",
  panel: "#0B111A",
  panelRaised: "#111A26",
  line: "#1C2736",
  lineBright: "#223044",
  node: "#334357",
  mint: "#3CEFB4",
  cyan: "#27BFF5",
  muted: "#9AA8BC",
  paper: "#F4F7F6",
  evergreen: "#0A7A5C",
  blue: "#0A70B0",
  white: "#F2F6F9",
};

const escapeXml = (value) => value.replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;");
const markPaths = (fill, gradientId, legFill = `url(#${gradientId})`) => `
  <g fill="${fill}">
    <path d="M88 390V122A34 34 0 0 1 156 122V390A34 34 0 0 1 88 390Z"/>
    <path d="M356 390V122A34 34 0 0 1 424 122V390A34 34 0 0 1 356 390Z"/>
    <path d="M88 280A101 101 0 0 1 290 280H222A33 33 0 0 0 156 280Z"/>
    <path d="M222 280A101 101 0 0 1 424 280H356A33 33 0 0 0 290 280Z"/>
  </g>
  <path fill="${legFill}" d="M222 280H290V390A34 34 0 0 1 222 390Z"/>`;

function svgDocument(width, height, title, body, defs = "", attributes = "") {
  return `<?xml version="1.0" encoding="UTF-8"?>
<svg xmlns="http://www.w3.org/2000/svg" width="${width}" height="${height}" viewBox="0 0 ${width} ${height}" role="img" aria-labelledby="title desc"${attributes}>
  <title id="title">${escapeXml(title)}</title>
  <desc id="desc">Headmesh identity artwork: the shared-leg arch mark represents two private meshes joined by a temporary grant.</desc>
  <defs>${defs}</defs>
  ${body}
</svg>
`;
}

function markSvg({ fill, start, end, title, background = "none" }) {
  const body = `${background === "none" ? "" : `<rect x="64" y="64" width="384" height="384" rx="82" fill="${background}"/>`}
    <g transform="translate(0 0)">${markPaths(fill, "markGradient")}</g>`;
  const defs = `<linearGradient id="markGradient" x1="222" y1="280" x2="290" y2="424" gradientUnits="userSpaceOnUse"><stop stop-color="${start}"/><stop offset="1" stop-color="${end}"/></linearGradient>`;
  return `<?xml version="1.0" encoding="UTF-8"?>
<svg xmlns="http://www.w3.org/2000/svg" width="512" height="512" viewBox="76 76 360 360" role="img" aria-labelledby="title">
  <title id="title">${escapeXml(title)}</title><defs>${defs}</defs>${body}
</svg>
`;
}

function lockupSvg({ dark }) {
  const fill = dark ? colors.white : colors.ink;
  const text = dark ? colors.white : colors.ink;
  const start = dark ? colors.mint : colors.evergreen;
  const end = dark ? colors.cyan : colors.blue;
  const defs = `<linearGradient id="lockupGradient" x1="222" y1="280" x2="290" y2="424" gradientUnits="userSpaceOnUse"><stop stop-color="${start}"/><stop offset="1" stop-color="${end}"/></linearGradient>`;
  const body = `<g transform="translate(30 0) scale(.40)">${markPaths(fill, "lockupGradient")}</g>
    <path transform="translate(254 73) scale(.82)" fill="${text}" d="${wordmarkPath}"/>`;
  return svgDocument(880, 200, `Headmesh wordmark for ${dark ? "dark" : "light"} backgrounds`, body, defs);
}

function appIconSvg({ light = false, shape = "rounded", gradient = false }) {
  const background = gradient ? "url(#appBackground)" : light ? colors.paper : colors.ink;
  const fill = gradient ? "#000000" : light ? colors.ink : colors.white;
  const start = gradient ? "#000000" : light ? colors.evergreen : colors.mint;
  const end = gradient ? "#000000" : light ? colors.blue : colors.cyan;
  let defs = `<linearGradient id="appGradient" x1="222" y1="280" x2="290" y2="424" gradientUnits="userSpaceOnUse"><stop stop-color="${start}"/><stop offset="1" stop-color="${end}"/></linearGradient>`;
  if (gradient) {
    defs += `<linearGradient id="appBackground" x1="0" y1="0" x2="512" y2="512" gradientUnits="userSpaceOnUse"><stop stop-color="${colors.mint}"/><stop offset="1" stop-color="${colors.cyan}"/></linearGradient>`;
  }
  const backgrounds = {
    rounded: `<rect width="512" height="512" rx="116" fill="${background}"/>`,
    circle: `<circle cx="256" cy="256" r="256" fill="${background}"/>`,
    square: `<rect width="512" height="512" fill="${background}"/>`,
  };
  if (!Object.hasOwn(backgrounds, shape)) throw new Error(`Unknown icon shape: ${shape}`);
  const body = `${backgrounds[shape]}<g>${markPaths(fill, "appGradient")}</g>`;
  const title = shape === "rounded" ? "Headmesh app icon" : `Headmesh ${shape} app icon`;
  return svgDocument(512, 512, `${title} ${gradient ? "on gradient" : light ? "on light" : "on dark"}`, body, defs);
}

const logoStyles = [
  {
    key: "gradient",
    label: "Gradient logo · transparent",
    colour: "gradient",
    foreground: "gradient",
    background: "transparent",
    palette: "dark",
  },
  {
    key: "black",
    label: "Black logo · transparent",
    colour: "black",
    foreground: "#000000",
    background: "transparent",
  },
  {
    key: "white",
    label: "White logo · transparent",
    colour: "white",
    foreground: "#FFFFFF",
    background: "transparent",
  },
  {
    key: "on-dark",
    label: "Shared-leg accent · for dark surfaces",
    colour: "brand",
    foreground: colors.white,
    background: "transparent",
    accent: "dark",
  },
  {
    key: "on-light",
    label: "Shared-leg accent · for light surfaces",
    colour: "brand",
    foreground: colors.ink,
    background: "transparent",
    accent: "light",
  },
  {
    key: "white-on-black",
    label: "White logo · black background",
    colour: "white",
    foreground: "#FFFFFF",
    background: "black",
  },
  {
    key: "black-on-white",
    label: "Black logo · white background",
    colour: "black",
    foreground: "#000000",
    background: "white",
  },
  {
    key: "gradient-on-black",
    label: "Gradient logo · black background",
    colour: "gradient",
    foreground: "gradient",
    background: "black",
    palette: "dark",
  },
  {
    key: "gradient-on-white",
    label: "Gradient logo · white background",
    colour: "gradient",
    foreground: "gradient",
    background: "white",
    palette: "light",
  },
  {
    key: "black-on-gradient",
    label: "Black logo · gradient background",
    colour: "black",
    foreground: "#000000",
    background: "gradient",
  },
];
const logoFormats = [
  { key: "mark", stem: "mark", label: "Standalone mark", width: 512, height: 512 },
  { key: "wordmark", stem: "wordmark", label: "Wordmark", width: 1000, height: 240 },
  { key: "horizontal", stem: "lockup-horizontal", label: "Horizontal lockup", width: 1200, height: 280 },
  { key: "vertical", stem: "lockup-vertical", label: "Vertical / stacked lockup", width: 720, height: 576 },
];

function logoAssetSvg(format, style) {
  const { width, height, key } = format;
  const [left, top, wordWidth, wordHeight] = wordmarkBounds;
  const palette = style.palette === "light" || style.accent === "light";
  const start = palette ? colors.evergreen : colors.mint;
  const end = palette ? colors.blue : colors.cyan;
  let defs = "";
  let markFill = style.foreground;
  let typeFill = style.foreground;
  if (style.foreground === "gradient") {
    defs += `<linearGradient id="markColour" x1="88" y1="88" x2="424" y2="424" gradientUnits="userSpaceOnUse"><stop stop-color="${start}"/><stop offset="1" stop-color="${end}"/></linearGradient>`;
    defs += `<linearGradient id="typeColour" x1="0" y1="0" x2="1" y2="1"><stop stop-color="${start}"/><stop offset="1" stop-color="${end}"/></linearGradient>`;
    markFill = "url(#markColour)";
    typeFill = "url(#typeColour)";
  }
  let legFill = markFill;
  if (style.accent) {
    defs += `<linearGradient id="sharedLeg" x1="222" y1="280" x2="290" y2="424" gradientUnits="userSpaceOnUse"><stop stop-color="${start}"/><stop offset="1" stop-color="${end}"/></linearGradient>`;
    legFill = "url(#sharedLeg)";
  }
  let body = "";
  if (style.background !== "transparent") {
    const backgrounds = { black: "#000000", white: "#FFFFFF", gradient: "url(#surfaceColour)" };
    if (!Object.hasOwn(backgrounds, style.background)) throw new Error(`Unknown logo background: ${style.background}`);
    if (style.background === "gradient") {
      defs += `<linearGradient id="surfaceColour" x1="0" y1="0" x2="${width}" y2="${height}" gradientUnits="userSpaceOnUse"><stop stop-color="${colors.mint}"/><stop offset="1" stop-color="${colors.cyan}"/></linearGradient>`;
    }
    body += `<rect width="${width}" height="${height}" fill="${backgrounds[style.background]}"/>`;
  }
  const number = (value) => Number(value.toFixed(6));
  const placeMark = (x, y, size) => {
    const scale = size / 336;
    return `<g transform="translate(${number(x - 88 * scale)} ${number(y - 88 * scale)}) scale(${number(scale)})">${markPaths(markFill, "sharedLeg", legFill)}</g>`;
  };
  const placeType = (x, y, targetWidth) =>
    `<g transform="translate(${number(x)} ${number(y)}) scale(${number(targetWidth / wordWidth)})"><path transform="translate(${-left} ${-top})" fill="${typeFill}" d="${wordmarkPath}"/></g>`;
  if (key === "mark") {
    body += placeMark(88, 88, 336);
  } else if (key === "wordmark") {
    body += placeType(80, (height - (840 * wordHeight) / wordWidth) / 2, 840);
  } else if (key === "horizontal") {
    body += placeMark(64, 60, 160);
    body += placeType(288, (height - (848 * wordHeight) / wordWidth) / 2, 848);
  } else if (key === "vertical") {
    body += placeMark(220, 64, 280);
    body += placeType(64, 408, 592);
  } else {
    throw new Error(`Unknown logo layout: ${key}`);
  }
  return svgDocument(
    width,
    height,
    `Headmesh ${format.label} — ${style.label}`,
    body,
    defs,
    ` data-background="${style.background}" data-layout="${key}" data-colour="${style.colour}"`,
  );
}

const pointSets = [
  [
    [0.15, 0.27],
    [0.34, 0.18],
    [0.55, 0.31],
    [0.82, 0.23],
    [0.25, 0.62],
    [0.48, 0.76],
    [0.73, 0.63],
    [0.86, 0.48],
  ],
  [
    [0.12, 0.32],
    [0.34, 0.22],
    [0.58, 0.36],
    [0.85, 0.25],
    [0.21, 0.65],
    [0.48, 0.77],
    [0.74, 0.62],
    [0.88, 0.48],
  ],
];
const edges = [
  [0, 1],
  [1, 2],
  [2, 3],
  [0, 4],
  [1, 4],
  [1, 5],
  [2, 5],
  [2, 6],
  [3, 6],
  [3, 7],
  [4, 5],
  [5, 6],
  [6, 7],
];

function networkMotif(x, y, width, height, prefix) {
  const gap = width * 0.16;
  const leftWidth = (width - gap) * 0.58;
  const rightWidth = width - gap - leftWidth;
  const boxes = [
    { x, y, w: leftWidth, h: height, label: "TENANT A", points: pointSets[0] },
    {
      x: x + leftWidth + gap,
      y: y + height * 0.16,
      w: rightWidth,
      h: height * 0.68,
      label: "TENANT B",
      points: pointSets[1],
    },
  ];
  let body = "";
  const endpoints = [];
  for (let index = 0; index < boxes.length; index += 1) {
    const box = boxes[index];
    const left = index === 0;
    body += `<rect x="${box.x.toFixed(1)}" y="${box.y.toFixed(1)}" width="${box.w.toFixed(1)}" height="${box.h.toFixed(1)}" rx="${Math.min(box.w, box.h) * 0.12}" fill="${colors.panel}" stroke="${colors.line}" stroke-width="2"/>`;
    body += `<text x="${(box.x + 20).toFixed(1)}" y="${(box.y + 30).toFixed(1)}" fill="${colors.muted}" font-family="'Geist Mono', 'Noto Sans Mono', monospace" font-size="12" letter-spacing="1.5">${box.label}</text>`;
    const pts = box.points.map(([px, py]) => [box.x + box.w * px, box.y + box.h * py]);
    for (const [a, b] of edges) {
      body += `<line x1="${pts[a][0].toFixed(1)}" y1="${pts[a][1].toFixed(1)}" x2="${pts[b][0].toFixed(1)}" y2="${pts[b][1].toFixed(1)}" stroke="${colors.lineBright}" stroke-width="2"/>`;
    }
    for (let point = 0; point < pts.length; point += 1) {
      const accent = (left && point === 7) || (!left && point === 0);
      body += `<circle cx="${pts[point][0].toFixed(1)}" cy="${pts[point][1].toFixed(1)}" r="${accent ? 8 : 5}" fill="${accent ? (left ? colors.mint : colors.cyan) : colors.node}"/>`;
    }
    endpoints.push(left ? pts[7] : pts[0]);
  }
  const [from, to] = endpoints;
  const gradientId = `${prefix}Grant`;
  body += `<line x1="${from[0].toFixed(1)}" y1="${from[1].toFixed(1)}" x2="${to[0].toFixed(1)}" y2="${to[1].toFixed(1)}" stroke="url(#${gradientId})" stroke-width="4" stroke-dasharray="12 10" stroke-linecap="round"/>`;
  const badgeWidth = Math.min(144, gap * 1.45);
  const bx = x + leftWidth + (gap - badgeWidth) / 2;
  const by = (from[1] + to[1]) / 2 - 17;
  body += `<rect x="${bx.toFixed(1)}" y="${by.toFixed(1)}" width="${badgeWidth.toFixed(1)}" height="34" rx="17" fill="#0E2A24" stroke="${colors.mint}" stroke-opacity=".55"/>`;
  body += `<text x="${(bx + badgeWidth / 2).toFixed(1)}" y="${(by + 22).toFixed(1)}" text-anchor="middle" fill="${colors.mint}" font-family="'Geist Mono', 'Noto Sans Mono', monospace" font-size="13">grant · 4h</text>`;
  return {
    body,
    defs: `<linearGradient id="${gradientId}" x1="${from[0]}" y1="${from[1]}" x2="${to[0]}" y2="${to[1]}" gradientUnits="userSpaceOnUse"><stop stop-color="${colors.mint}"/><stop offset="1" stop-color="${colors.cyan}"/></linearGradient>`,
  };
}

function socialCard(width, height, variant) {
  const isOg = variant === "og";
  const isX = variant === "x";
  const titleY = isOg ? 303 : isX ? 230 : 190;
  const titleSize = isOg ? 61 : isX ? 67 : 58;
  const infoY = isOg ? 448 : isX ? 375 : 312;
  const footerY = height - (isOg ? 48 : 42);
  const motifX = isOg ? 660 : isX ? 842 : 998;
  const motifY = isOg ? 133 : isX ? 58 : 34;
  const motifW = isOg ? 478 : isX ? 575 : 520;
  const motifH = isOg ? 346 : isX ? 368 : 265;
  const motif = networkMotif(motifX, motifY, motifW, motifH, `net-${variant}-`);
  const body = `<rect width="${width}" height="${height}" fill="${colors.ink}"/>
    <circle cx="${width * 0.82}" cy="${height * 0.03}" r="${height * 0.56}" fill="${colors.mint}" opacity=".035"/>
    <g transform="translate(64 64) scale(.18)">${markPaths(colors.white, `card-${variant}-mark`)}</g>
    <path transform="translate(158 89) scale(.54)" fill="${colors.white}" d="${wordmarkPath}"/>
    <text x="72" y="${titleY}" fill="${colors.white}" font-family="'Bricolage Grotesque', 'Geist', Arial, sans-serif" font-size="${titleSize}" font-weight="800" letter-spacing="-0.025em">Share machines,</text>
    <text x="72" y="${titleY + titleSize * 1.18}" fill="${colors.white}" font-family="'Bricolage Grotesque', 'Geist', Arial, sans-serif" font-size="${titleSize}" font-weight="800" letter-spacing="-0.025em">not your network.</text>
    <text x="76" y="${infoY}" fill="${colors.muted}" font-family="'Geist', Arial, sans-serif" font-size="${isOg ? 21 : 22}">Temporary grants. Separate tenants. Yours to run.</text>
    <line x1="72" y1="${footerY - 30}" x2="${width - 72}" y2="${footerY - 30}" stroke="${colors.line}" stroke-width="2"/>
    <text x="74" y="${footerY}" fill="${colors.muted}" font-family="'Geist Mono', 'Noto Sans Mono', monospace" font-size="14" letter-spacing="1.2">HEADMESH.DEV</text>
    ${motif.body}`;
  const defs = `<linearGradient id="card-${variant}-mark" x1="222" y1="280" x2="290" y2="424" gradientUnits="userSpaceOnUse"><stop stop-color="${colors.mint}"/><stop offset="1" stop-color="${colors.cyan}"/></linearGradient>${motif.defs}`;
  return svgDocument(width, height, "Headmesh — Share machines, not your network", body, defs);
}

async function save(relativePath, data) {
  const target = path.join(root, relativePath);
  await mkdir(path.dirname(target), { recursive: true });
  await writeFile(target, data);
}

async function renderPng(svg, relativePath, width) {
  const image = new Resvg(svg, { fitTo: { mode: "width", value: width } }).render().asPng();
  await save(relativePath, image);
  return image;
}

function node(type, style, children, extra = {}) {
  return { type, props: { style, children, ...extra } };
}

async function renderSocialPng(svg, relativePath, width, height, variant) {
  const isOg = variant === "og";
  const isX = variant === "x";
  const titleY = isOg ? 303 : isX ? 230 : 190;
  const titleSize = isOg ? 61 : isX ? 67 : 58;
  const infoY = isOg ? 448 : isX ? 375 : 312;
  const infoSize = isOg ? 21 : 22;
  const footerY = height - (isOg ? 48 : 42);
  const motifX = isOg ? 660 : isX ? 842 : 998;
  const motifY = isOg ? 133 : isX ? 58 : 34;
  const motifW = isOg ? 478 : isX ? 575 : 520;
  const motifH = isOg ? 346 : isX ? 368 : 265;
  const gap = motifW * 0.16;
  const leftWidth = (motifW - gap) * 0.58;
  const rightWidth = motifW - gap - leftWidth;
  const rightY = motifY + motifH * 0.16;
  const rightHeight = motifH * 0.68;
  const leftPoint = [motifX + leftWidth * pointSets[0][7][0], motifY + motifH * pointSets[0][7][1]];
  const rightPoint = [
    motifX + leftWidth + gap + rightWidth * pointSets[1][0][0],
    rightY + rightHeight * pointSets[1][0][1],
  ];
  const badgeWidth = Math.min(144, gap * 1.45);
  const badgeX = motifX + leftWidth + (gap - badgeWidth) / 2;
  const badgeY = (leftPoint[1] + rightPoint[1]) / 2 - 17;
  const baseSvg = svg.replace(/<text\b[^>]*>[\s\S]*?<\/text>/g, "");
  const svgImage = `data:image/svg+xml;base64,${Buffer.from(baseSvg).toString("base64")}`;
  const text = (content, style) => node("div", { position: "absolute", display: "flex", ...style }, content);
  const children = [
    node("img", { position: "absolute", left: 0, top: 0, width, height }, undefined, { src: svgImage, width, height }),
    text("TENANT A", {
      left: motifX + 20,
      top: motifY + 18,
      fontFamily: "Geist Mono",
      fontSize: 12,
      lineHeight: "16px",
      letterSpacing: 1.5,
      color: colors.muted,
    }),
    text("TENANT B", {
      left: motifX + leftWidth + gap + 20,
      top: rightY + 18,
      fontFamily: "Geist Mono",
      fontSize: 12,
      lineHeight: "16px",
      letterSpacing: 1.5,
      color: colors.muted,
    }),
    text("grant · 4h", {
      left: badgeX,
      top: badgeY + 6,
      width: badgeWidth,
      height: 22,
      alignItems: "center",
      justifyContent: "center",
      fontFamily: "Geist Mono",
      fontSize: 13,
      lineHeight: "18px",
      color: colors.mint,
    }),
    text("Share machines,", {
      left: 72,
      top: titleY - titleSize,
      fontFamily: "Bricolage Grotesque",
      fontSize: titleSize,
      fontWeight: 800,
      lineHeight: `${titleSize * 1.18}px`,
      letterSpacing: -titleSize * 0.025,
      color: colors.white,
    }),
    text("not your network.", {
      left: 72,
      top: titleY - titleSize + titleSize * 1.18,
      fontFamily: "Bricolage Grotesque",
      fontSize: titleSize,
      fontWeight: 800,
      lineHeight: `${titleSize * 1.18}px`,
      letterSpacing: -titleSize * 0.025,
      color: colors.white,
    }),
    text("Temporary grants. Separate tenants. Yours to run.", {
      left: 76,
      top: infoY - infoSize,
      fontFamily: "Geist",
      fontSize: infoSize,
      lineHeight: `${infoSize * 1.35}px`,
      color: colors.muted,
    }),
    text("HEADMESH.DEV", {
      left: 74,
      top: footerY - 18,
      fontFamily: "Geist Mono",
      fontSize: 14,
      lineHeight: "18px",
      letterSpacing: 1.2,
      color: colors.muted,
    }),
  ];
  const tree = node(
    "div",
    { position: "relative", display: "flex", width, height, backgroundColor: colors.ink },
    children,
  );
  const renderedSvg = await satori(tree, { width, height, fonts: socialFonts });
  const png = new Resvg(renderedSvg, { fitTo: { mode: "width", value: width } }).render().asPng();
  await save(relativePath, png);
  return png;
}

async function copyReference(source, destination) {
  const target = path.join(root, destination);
  await mkdir(path.dirname(target), { recursive: true });
  await copyFile(path.join(referenceRoot, source), target);
}

function makeIco(entries) {
  const header = Buffer.alloc(6 + entries.length * 16);
  header.writeUInt16LE(0, 0);
  header.writeUInt16LE(1, 2);
  header.writeUInt16LE(entries.length, 4);
  let offset = header.length;
  entries.forEach(({ size, png }, index) => {
    const start = 6 + index * 16;
    header.writeUInt8(size === 256 ? 0 : size, start);
    header.writeUInt8(size === 256 ? 0 : size, start + 1);
    header.writeUInt8(0, start + 2);
    header.writeUInt8(0, start + 3);
    header.writeUInt16LE(1, start + 4);
    header.writeUInt16LE(32, start + 6);
    header.writeUInt32LE(png.length, start + 8);
    header.writeUInt32LE(offset, start + 12);
    offset += png.length;
  });
  return Buffer.concat([header, ...entries.map(({ png }) => png)]);
}

const darkMark = markSvg({
  fill: colors.white,
  start: colors.mint,
  end: colors.cyan,
  title: "Headmesh D4 Shared Leg mark on dark backgrounds",
});
const lightMark = markSvg({
  fill: colors.ink,
  start: colors.evergreen,
  end: colors.blue,
  title: "Headmesh D4 Shared Leg mark on light backgrounds",
});
const darkIcon = appIconSvg({});
const lightIcon = appIconSvg({ light: true });
const shapedIcons = ["circle", "square"].flatMap((shape) =>
  [false, true].map((light) => ({
    name: `app-icon-${shape}-on-${light ? "light" : "dark"}`,
    svg: appIconSvg({ light, shape }),
  })),
);
shapedIcons.push(
  ...["rounded", "circle", "square"].map((shape) => ({
    name: shape === "rounded" ? "app-icon-on-gradient" : `app-icon-${shape}-on-gradient`,
    svg: appIconSvg({ shape, gradient: true }),
  })),
);
const logoAssets = logoFormats.flatMap((format) =>
  logoStyles.map((style) => {
    const name = `${format.stem}-${style.key}`;
    // Preserve the original tightly cropped mark aliases used by the site.
    const svg =
      format.key === "mark" && style.key === "on-dark"
        ? darkMark
        : format.key === "mark" && style.key === "on-light"
          ? lightMark
          : logoAssetSvg(format, style);
    return {
      name,
      layout: format.key,
      colour: style.colour,
      background: style.background,
      style: style.key,
      label: `${format.label} · ${style.label}`,
      width: format.width,
      height: format.height,
      svg,
    };
  }),
);
const logoManifest = {
  format: 1,
  count: logoAssets.length,
  assets: logoAssets.map(({ svg: _svg, ...asset }) => ({
    ...asset,
    svg: `${asset.name}.svg`,
    png: `${asset.name}.png`,
  })),
};
const og = socialCard(1200, 630, "og");
const xBanner = socialCard(1500, 500, "x");
const linkedin = socialCard(1584, 396, "linkedin");

await Promise.all([
  ...logoAssets.map(({ name, svg }) => save(`logos/${name}.svg`, svg)),
  save("logos/manifest.json", `${JSON.stringify(logoManifest, null, 2)}\n`),
  save("logos/lockup-on-dark.svg", lockupSvg({ dark: true })),
  save("logos/lockup-on-light.svg", lockupSvg({ dark: false })),
  save("icons/app-icon.svg", darkIcon),
  save("icons/app-icon-on-light.svg", lightIcon),
  save("icons/favicon.svg", darkIcon),
  ...shapedIcons.map(({ name, svg }) => save(`icons/${name}.svg`, svg)),
  save("social/og-image-1200x630.svg", og),
  save("social/x-banner-1500x500.svg", xBanner),
  save("social/linkedin-banner-1584x396.svg", linkedin),
]);

const icoEntries = [];
for (const size of [16, 32, 48]) {
  const png = await renderPng(darkIcon, `icons/favicon-${size}.png`, size);
  icoEntries.push({ size, png });
}
await save("icons/favicon.ico", makeIco(icoEntries));
await Promise.all([
  renderPng(darkIcon, "icons/apple-touch-icon.png", 180),
  renderPng(darkIcon, "icons/icon-192.png", 192),
  renderPng(darkIcon, "icons/icon-512.png", 512),
  renderPng(lightIcon, "icons/icon-192-light.png", 192),
  renderPng(lightIcon, "icons/icon-512-light.png", 512),
  ...shapedIcons.map(({ name, svg }) => renderPng(svg, `icons/${name}.png`, 512)),
  ...logoAssets.map(({ name, svg, width }) => renderPng(svg, `logos/${name}.png`, width)),
  renderSocialPng(og, "social/og-image-1200x630.png", 1200, 630, "og"),
  renderSocialPng(xBanner, "social/x-banner-1500x500.png", 1500, 500, "x"),
  renderSocialPng(linkedin, "social/linkedin-banner-1584x396.png", 1584, 396, "linkedin"),
]);

// Keep the original final-size exports as the canonical raster files. The
// accompanying SVGs remain editable reconstructions of the transcript brief.
await Promise.all([
  copyReference("banner-x-1500x500.png", "social/x-banner-1500x500.png"),
  copyReference("og-card-grant-1200x630.png", "social/og-image-1200x630.png"),
]);

await save(
  "tokens.css",
  `:root {\n  --hm-ink: ${colors.ink};\n  --hm-panel: ${colors.panel};\n  --hm-panel-raised: ${colors.panelRaised};\n  --hm-line: ${colors.line};\n  --hm-line-bright: ${colors.lineBright};\n  --hm-node: ${colors.node};\n  --hm-mint: ${colors.mint};\n  --hm-cyan: ${colors.cyan};\n  --hm-muted: ${colors.muted};\n  --hm-paper: ${colors.paper};\n  --hm-evergreen: ${colors.evergreen};\n  --hm-blue: ${colors.blue};\n  --hm-white: ${colors.white};\n}\n`,
);

await save(
  "README.md",
  `# Headmesh brand kit — D4 Shared Leg\n\nThis kit follows the selected D4 “Arch · Shared Leg” mark and final wordmark recorded in the Claude Design transcript. The mark uses the transcript's exact path geometry. The shared-leg accent retains the original gradient axis. The wordmark uses the outlined lowercase Bricolage Grotesque ExtraBold path (weight 800, tracking -2.5%) from src/components/Wordmark.astro.\n\n## Contents\n\n- [logos/](logos/index.html): 40 SVG/PNG pairs covering standalone marks, wordmarks, horizontal lockups, and vertical lockups. Includes gradient, true black, and true white artwork; transparent, black, white, and gradient backgrounds; and the original shared-leg accent colourways. See [logo usage](logos/README.md).\n- icons/: rounded, circular, and square app icons on dark, light, and gradient backgrounds, plus favicons, Apple touch icon, and ICO bundle.\n- social/: editable SVGs and PNGs. The X banner and home OG card are byte-for-byte copies of banner-x-1500x500.png and og-card-grant-1200x630.png in brand-reference/headmesh-process/. The D4 LinkedIn banner is a local vector reconstruction using the selected mark, outlined wordmark, and bundled fonts; the supplied C1 LinkedIn exploration is preserved separately as a candidate.\n- tokens.css: dark and light color custom properties from the transcript.\n- preview.html: local visual contact sheet.\n\nThe descriptive filenames in brand-reference/headmesh-process/ map to the original hm1–hm9 download names in that directory's README. The full 585-file export from the isolated Claude workspace was not available; this repo contains the original downloaded references and reconstructed vector assets.\n\n## Regenerate\n\nFrom web/marketing/:\n\n    pnpm exec node scripts/generate-brand-kit.mjs\n\nThe generator reads the outlined wordmark component and transcript reference images, then writes the versioned kit under public/brand/headmesh-v1/.\n`,
);

await save(
  "preview.html",
  `<!doctype html>\n<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Headmesh brand kit — D4 Shared Leg</title><style>body{margin:0;padding:32px;background:#252d38;color:#d9e2ec;font:14px/1.5 Geist,Arial,sans-serif}h1{font:700 24px/1.2 'Bricolage Grotesque',Geist,Arial,sans-serif;margin:0 0 24px}.row{display:grid;grid-template-columns:180px 1fr 1fr;gap:16px;align-items:center;margin:12px 0}.sample{min-height:150px;padding:24px;border-radius:12px;display:flex;align-items:center;justify-content:center}.dark{background:#070b12}.light{background:#f4f7f6}.logo{max-width:100%;max-height:110px}.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(300px,1fr));gap:16px;margin-top:28px}.card{width:100%;border-radius:10px}.label{font:12px/1.5 'Geist Mono',monospace;color:#aab7c5}</style></head><body><h1>Headmesh · D4 “Shared Leg”</h1><p><a style="color:#3cefb4" href="logos/index.html">Complete logo system · SVG + PNG</a></p><div class="row"><div class="label">Horizontal lockup</div><div class="sample dark"><img class="logo" src="logos/lockup-on-dark.svg" alt="Headmesh logo on dark"></div><div class="sample light"><img class="logo" src="logos/lockup-on-light.svg" alt="Headmesh logo on light"></div></div><div class="row"><div class="label">Standalone mark</div><div class="sample dark"><img class="logo" src="logos/mark-on-dark.svg" alt="Headmesh mark on dark"></div><div class="sample light"><img class="logo" src="logos/mark-on-light.svg" alt="Headmesh mark on light"></div></div><div class="cards"><img class="card" src="social/og-image-1200x630.png" alt="Open Graph card"><img class="card" src="social/x-banner-1500x500.png" alt="X banner"><img class="card" src="social/linkedin-banner-1584x396.png" alt="LinkedIn banner"></div></body></html>\n`,
);

await save(
  "icons/README.md",
  `# Headmesh D4 icon shapes

All shapes use the same 512 × 512 canvas, shared-leg mark, gradient axis, and mark placement.

| Shape | Theme | SVG | PNG |
| --- | --- | --- | --- |
| Rounded square | Dark | [SVG](app-icon.svg) | [PNG](icon-512.png) |
| Rounded square | Light | [SVG](app-icon-on-light.svg) | [PNG](icon-512-light.png) |
| Circle | Dark | [SVG](app-icon-circle-on-dark.svg) | [PNG](app-icon-circle-on-dark.png) |
| Circle | Light | [SVG](app-icon-circle-on-light.svg) | [PNG](app-icon-circle-on-light.png) |
| Square | Dark | [SVG](app-icon-square-on-dark.svg) | [PNG](app-icon-square-on-dark.png) |
| Square | Light | [SVG](app-icon-square-on-light.svg) | [PNG](app-icon-square-on-light.png) |
| Rounded square | Gradient | [SVG](app-icon-on-gradient.svg) | [PNG](app-icon-on-gradient.png) |
| Circle | Gradient | [SVG](app-icon-circle-on-gradient.svg) | [PNG](app-icon-circle-on-gradient.png) |
| Square | Gradient | [SVG](app-icon-square-on-gradient.svg) | [PNG](app-icon-square-on-gradient.png) |

The light theme intentionally uses evergreen-to-blue (#0A7A5C → #0A70B0) on paper (#F4F7F6).
The dark theme uses mint-to-cyan (#3CEFB4 → #27BFF5) on ink (#070B12).
The shared leg is fully opaque in both themes; the contrast comes from the palette, not reduced opacity.
Gradient backgrounds use mint-to-cyan with a solid black mark, including its shared leg.
PNG corners outside the rounded and circular backgrounds remain transparent. Squares have sharp, opaque corners.
`,
);

await save(
  "logos/README.md",
  `# Headmesh logo system

Open [index.html](index.html) to compare and download all 40 SVG/PNG pairs. Typography is outlined; no fonts are required.

| Layout | Filename prefix | PNG canvas |
| --- | --- | --- |
| Standalone mark | mark- | 512 × 512 |
| Wordmark | wordmark- | 1000 × 240 |
| Horizontal lockup | lockup-horizontal- | 1200 × 280 |
| Vertical / stacked lockup | lockup-vertical- | 720 × 576 |

Each layout has these ten variants:

${logoStyles.map((style) => `- **${style.key}**: ${style.label}.`).join("\n")}

## Colour and background rules

- Black and white artwork/backgrounds use true #000000 and #FFFFFF.
- The full-colour gradient covers the whole mark and wordmark. It uses mint-to-cyan (#3CEFB4 → #27BFF5) for dark surfaces; gradient-on-white uses deeper evergreen-to-blue (#0A7A5C → #0A70B0) for contrast.
- on-dark and on-light preserve the original shared-leg accent: white/ink outer shapes with colour only on the shared leg. Their backgrounds are transparent. The original mark aliases retain their tight crop; other exports include consistent canvas padding.
- black-on-gradient has an opaque mint-to-cyan background and a solid black logo. Background colours are included in both SVG and PNG files.
- Transparent exports preserve alpha. Use white artwork on dark surfaces and black artwork on light surfaces.
- The two original lockup-on-dark/light SVGs remain available as compatibility exports. New lockup-horizontal-* filenames identify the complete current set.

manifest.json records every current logo's layout, colour, background, filename, and native export size.
`,
);

const logoCards = logoAssets
  .map((asset) => {
    const light = asset.colour === "black" || asset.style === "on-light" || asset.background === "white";
    return `<article data-layout="${asset.layout}" data-colour="${asset.colour}" data-background="${asset.background}"><h2>${escapeXml(asset.label)}</h2><div class="stage ${light ? "light" : "dark"}"><img src="${asset.name}.svg" width="${asset.width}" height="${asset.height}" alt="${escapeXml(asset.label)}"></div><p><a href="${asset.name}.svg" download>SVG</a> · <a href="${asset.name}.png" download>PNG · ${asset.width} × ${asset.height}</a></p></article>`;
  })
  .join("");
await save(
  "logos/index.html",
  `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Headmesh logo system</title><style>
:root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;background:#070b12;color:#f2f6f9;font:16px/1.5 system-ui}main{max-width:1280px;margin:auto;padding:24px}h1{font-size:32px}h2{font-size:16px;margin-top:0}p{color:#9aa8bc}a{color:#3cefb4}.filters{display:flex;gap:16px;flex-wrap:wrap;margin:24px 0}label{display:grid;gap:6px}select{font:inherit;padding:8px;background:#111a26;color:#f2f6f9;border:1px solid #334357;border-radius:8px}.grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:20px}article{min-width:0;border:1px solid #1c2736;border-radius:12px;padding:20px}article[hidden]{display:none}.stage{display:flex;align-items:center;justify-content:center;min-height:240px;padding:16px;border-radius:8px}.dark{background:repeating-conic-gradient(#0b111a 0% 25%,#111a26 0% 50%) 0 0/24px 24px}.light{background:repeating-conic-gradient(#f4f7f6 0% 25%,#e9efee 0% 50%) 0 0/24px 24px}img{display:block;max-width:100%;width:100%;height:208px;object-fit:contain}article p{margin-bottom:0}@media(max-width:640px){.grid{grid-template-columns:1fr}main{padding:16px}.filters{gap:12px}select{max-width:100%}}
</style></head><body><main><h1>Headmesh · logo system</h1><p>The selected D4 shared-leg geometry and outlined Bricolage wordmark. Four layouts, ten colour/background combinations, each in SVG and PNG.</p><p>Checkerboards indicate transparency. Gradient artwork uses a deeper green/blue palette on white; the bright mint/cyan palette is used on black. <a href="README.md">Usage notes</a> · <a href="../preview.html">Brand kit</a></p><div class="filters"><label>Layout<select id="layout" aria-label="Layout"><option value="all">All layouts</option>${logoFormats.map((format) => `<option value="${format.key}">${format.label}</option>`).join("")}</select></label><label>Logo colour<select id="colour" aria-label="Logo colour"><option value="all">All colours</option><option value="gradient">Gradient</option><option value="black">Black</option><option value="white">White</option><option value="brand">Shared-leg accent</option></select></label><label>Background<select id="background" aria-label="Background"><option value="all">All backgrounds</option><option value="transparent">Transparent</option><option value="black">Black</option><option value="white">White</option><option value="gradient">Gradient</option></select></label></div><p id="count" aria-live="polite">40 variants</p><div class="grid">${logoCards}</div></main><script>const filters=[...document.querySelectorAll('select')];for(const filter of filters)filter.addEventListener('change',()=>{let count=0;for(const card of document.querySelectorAll('article')){card.hidden=!filters.every(filter=>filter.value==='all'||card.dataset[filter.id]===filter.value);if(!card.hidden)count++}document.getElementById('count').textContent=count+(count===1?' variant':' variants')});</script></body></html>\n`,
);

console.log(`Generated Headmesh D4 asset kit at ${root}`);
