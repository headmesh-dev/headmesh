"""Generate a complete 13-variant kit with outlined type and the recovered final grant motif.

The initial build.py was omitted from the transcripts. Its export schema and non-recorded
layout values below are reconstructed, not recovered source. Provenance is in manifest.json.
"""

import html
import json
from pathlib import Path

from marks import BLUE, CYAN, EVERGREEN, INK, MARKS, MINT, PAPER, RECORDED_BBOXES, _paint
from motif import constellation
from textpath import fmt, text_paths

ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / "out/headmesh-brand"
FG, MUTED, LINE = "#F2F6F9", "#9AA9B8", "#1C2736"
FONT_SPECS = {
    "A": ("headmesh", "Geist-600.ttf", -0.035, 4),
    "B": ("Headmesh", "Geist-700.ttf", -0.03, 4),
    "C": ("HEADMESH", "SpaceGrotesk-700.ttf", 0.04, None),
    "D": ("headmesh", "BricolageGrotesque-800.ttf", -0.025, None),
}
FOLDERS = {"A": "A-fullmesh", "B": "B-head-node", "C": "C-weave", "D": "D-arch"}
SLUGS = {
    "A1": "solid", "A2": "schematic", "A3": "pillars", "B1": "glyph", "B2": "node-h", "B3": "badge",
    "C1": "weave", "C2": "net", "C3": "ribbon", "D1": "monogram", "D2": "two-heads", "D3": "tunnel", "D4": "shared-leg",
}


def document(width, height, key, title, body, defs="", viewbox=None):
    viewbox = viewbox or f"0 0 {width} {height}"
    return f'''<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" viewBox="{viewbox}" role="img" aria-labelledby="title">
<title id="title">{html.escape(key + ' · ' + title)}</title><defs>{defs}</defs>{body}</svg>\n'''


def mark_at(key, x, y, size, paint="grad", foreground=FG, pid="mark"):
    function = MARKS[key][2]
    kwargs = {"paint": paint, "pid": pid}
    if key == "D4":
        kwargs["fg"] = foreground
    defs, body = function(**kwargs)
    left, top, right, bottom = RECORDED_BBOXES[key]
    factor = size / max(right - left, bottom - top)
    tx = x + size / 2 - (left + right) / 2 * factor
    ty = y + size / 2 - (top + bottom) / 2 * factor
    return defs, f'<g transform="translate({tx:.6f} {ty:.6f}) scale({factor:.8f})">{body}</g>'


def wordmark_at(key, size, x, y, foreground=FG, accent="grad", split=True, pid="wordmark"):
    content, font, tracking, cut = FONT_SPECS[key[0]]
    initial = text_paths(content, font, size, tracking=tracking)
    left, top, right, bottom = initial["bounds"]
    result = text_paths(content, font, size, tracking=tracking, x0=x - left, baseline=y - top,
                        split=cut if split else None)
    paint, defs = _paint(accent, pid)
    if cut is not None and split:
        body = f'<path fill="{foreground}" d="{fmt(result["d_a"])}"/><path fill="{paint}" d="{fmt(result["d_b"])}"/>'
    else:
        body = f'<path fill="{foreground}" d="{fmt(result["d"])}"/>'
    return defs, body, right - left, bottom - top


def line(text, font, size, x, baseline, color, tracking=0):
    shape = text_paths(text, font, size, tracking=tracking, x0=x, baseline=baseline)
    return f'<path fill="{color}" d="{fmt(shape["d"])}"/>'


def lockup(key, light=False, stacked=False, mono=None):
    fg = INK if light else FG
    paint = "light-grad" if light else "grad"
    if mono:
        fg = paint = mono
    if stacked:
        width, height = 640, 460
        defs, body = mark_at(key, 218, 26, 204, paint, fg, "stack-mark")
        wd, wb, tw, th = wordmark_at(key, 98, 0, 0, fg, paint, not mono, "stack-text")
        body += f'<g transform="translate({(width - tw) / 2:.4f} 313)">{wb}</g>'
    else:
        size = 82
        wd, wb, tw, th = wordmark_at(key, size, 116, (128 - 71.4 * size / 100) / 2, fg, paint, not mono)
        # Align each family's visible type bounds to the mark's centre.
        wd, wb, tw, th = wordmark_at(key, size, 116, (128 - th) / 2, fg, paint, not mono)
        width, height = round(132 + tw), 128
        defs, body = mark_at(key, 16, 26, 76, paint, fg, "horizontal-mark")
        body += wb
    return width, height, document(width, height, key, "Outlined lockup", body, defs + wd)


def social_card(key, width, height, kind):
    if kind == "og":
        margin, mark_size, wm_size = 80, 58, 54
        defs, body = mark_at(key, margin, 80, mark_size, "grad", FG, "brand-mark")
        wd, wb, _, _ = wordmark_at(key, wm_size, margin + 80, 90, FG, pid="brand-type")
        body += wb
        # These motif/baseline formulas are preserved verbatim from the recorded replacements.
        cd, cb = constellation(width * 0.56, height * 0.13, width * 0.37, height * 0.36, seed=11)
        body += cb
        y, headline_size = height * 0.545, 74
        for index, content in enumerate(["Share machines,", "not your network."]):
            body += line(content, "BricolageGrotesque-800.ttf", headline_size, margin,
                         y + index * headline_size * 1.06, FG, -0.028)
        body += line("Open, self-hostable, multi-tenant networking on Headscale and WireGuard.",
                     "Geist-400.ttf", 25, margin, y + headline_size * 1.06 + height * 0.098, MUTED)
        footer_y = height - margin * 0.92
        body += f'<rect x="{margin}" y="{footer_y - height * 0.068:.1f}" width="{width - 2 * margin}" height="2" fill="{LINE}"/>'
        domain = text_paths("headmesh.dev", "GeistMono-500.ttf", 24, x0=margin, baseline=footer_y)
        domain_fill, domain_defs = _paint("grad", "domain")
        body += f'<path fill="{domain_fill}" d="{fmt(domain["d"])}"/>'
        right_text = "Open source · Self-hostable · Multi-tenant"
        right = text_paths(right_text, "Geist-400.ttf", 23)
        body += line(right_text, "Geist-400.ttf", 23, width - margin - right["advance"], footer_y, MUTED)
        defs += wd + cd + domain_defs
    else:
        margin = 100 if kind == "x" else 80
        mark_size = height * 0.175
        defs, body = mark_at(key, margin, height * 0.335, mark_size, "grad", FG, "banner-mark")
        content, font, tracking, _ = FONT_SPECS[key[0]]
        initial = text_paths(content, font, height * 0.16, tracking=tracking)
        top = height * 0.475 + initial["bounds"][1]
        wd, wb, _, _ = wordmark_at(key, height * 0.16, margin + height * 0.24, top, FG, pid="banner-type")
        body += wb + line("Share machines, not your network.", "Geist-400.ttf", height * 0.068,
                          margin, height * 0.676, MUTED)
        cd, cb = constellation(width * 0.52, height * 0.16, width * 0.40, height * 0.68, seed=5)
        defs += wd + cd
        body += cb
    body = f'<rect width="{width}" height="{height}" fill="{INK}"/>' + body
    return width, height, document(width, height, key, "Share machines, not your network.", body, defs)


def assets_for(key):
    assets = {}
    for name, paint, fg in [
        ("mark-gradient", "grad", None), ("mark-on-dark", "grad", FG),
        ("mark-on-light", "light-grad", INK), ("mark-white", FG, FG),
        ("mark-ink", INK, INK), ("mark-mint", MINT, MINT),
    ]:
        kwargs = {"paint": paint, "pid": key.lower()}
        if key == "D4":
            kwargs["fg"] = fg
        defs, body = MARKS[key][2](**kwargs)
        assets[name] = (512, 512, document(512, 512, key, name, body, defs))
    for name, fg, accent, split in [
        ("wordmark-on-dark", FG, "grad", True), ("wordmark-on-light", INK, "light-grad", True),
        ("wordmark-white", FG, FG, False), ("wordmark-ink", INK, INK, False),
        ("wordmark-gradient", MINT, "grad", True),
    ]:
        defs, body, w, h = wordmark_at(key, 100, 6, 6, fg, accent, split)
        assets[name] = (round(w + 12), round(h + 12), document(round(w + 12), round(h + 12), key, name, body, defs))
    for stacked in (False, True):
        stem = "lockup-stacked" if stacked else "lockup-horizontal"
        for name, light, mono in [("on-dark", False, None), ("on-light", True, None), ("white", False, FG), ("ink", True, INK)]:
            assets[f"{stem}-{name}"] = lockup(key, light=light, stacked=stacked, mono=mono)
    for name, background, paint, fg in [
        ("app-icon-ink", INK, "grad", FG), ("app-icon-gradient", "gradient", INK, INK),
        ("app-icon-paper", PAPER, "light-grad", INK),
    ]:
        defs, body = mark_at(key, 88, 88, 336, paint, fg, "app-mark")
        if background == "gradient":
            fill, gradient = _paint("grad", "background")
            defs += gradient
        else:
            fill = background
        body = f'<rect width="512" height="512" rx="116" fill="{fill}"/>' + body
        assets[name] = (512, 512, document(512, 512, key, name, body, defs))
    assets["favicon"] = assets["app-icon-ink"]
    for kind, filename, width, height in [("og", "og-1200x630", 1200, 630), ("x", "banner-x-1500x500", 1500, 500), ("linkedin", "banner-linkedin-1584x396", 1584, 396)]:
        assets[filename] = social_card(key, width, height, kind)
    if len(assets) != 26:
        raise RuntimeError(f"Unexpected SVG schema: {key}: {len(assets)}")
    return assets


def main():
    jobs, variants, bbox_jobs = [], [], []
    temporary = ROOT / "tmp"
    temporary.mkdir(exist_ok=True)
    for key, (family, label, _) in MARKS.items():
        directory = OUT / FOLDERS[key[0]] / f"{key}-{SLUGS[key]}"
        (directory / "svg").mkdir(parents=True, exist_ok=True)
        (directory / "png").mkdir(exist_ok=True)
        assets = assets_for(key)
        for name, (_, _, data) in assets.items():
            (directory / "svg" / f"{name}.svg").write_text(data)
        raw = temporary / f"bbox-{key}.svg"
        defs, body = MARKS[key][2](paint="#000000", pid=key.lower())
        raw.write_text(document(512, 512, key, "Bounding box probe", body, defs))
        bbox_jobs.append({"in": str(raw.relative_to(ROOT)), "out": f"tmp/bbox-{key}.png", "w": 512, "h": 512})

        def png(source, output, width, height=None):
            sw, sh, _ = assets[source]
            jobs.append({"in": str((directory / "svg" / f"{source}.svg").relative_to(ROOT)),
                         "out": str((directory / "png" / f"{output}.png").relative_to(ROOT)),
                         "w": width, "h": height or round(width * sh / sw), "scale": 1})

        for size in (16, 32, 48, 64):
            png("favicon", f"favicon-{size}", size, size)
        png("app-icon-ink", "apple-touch-icon-180", 180, 180)
        for size in (192, 512):
            png("app-icon-ink", f"icon-{size}", size, size)
        for theme in ("ink", "gradient", "paper"):
            png(f"app-icon-{theme}", f"app-icon-{theme}-1024", 1024, 1024)
        png("mark-on-dark", "mark-transparent-512", 512, 512)
        for theme in ("dark", "light"):
            png(f"lockup-horizontal-on-{theme}", f"lockup-horizontal-on-{theme}-1200", 1200)
            png(f"lockup-stacked-on-{theme}", f"lockup-stacked-on-{theme}-512", 512)
        for filename, width, height in [("og-1200x630", 1200, 630), ("banner-x-1500x500", 1500, 500), ("banner-linkedin-1584x396", 1584, 396)]:
            png(filename, filename, width, height)
        variants.append({"key": key, "family": family, "label": label,
                         "directory": str(directory.relative_to(ROOT)), "svg_count": len(assets), "png_count": 18,
                         "recorded_bbox": RECORDED_BBOXES[key],
                         "geometry_evidence": "recovered function" if key in ("A1", "D4") else "reconstructed base with recorded edits and bounding box",
                         "wordmark": FONT_SPECS[key[0]]})
    if len(jobs) != 234:
        raise RuntimeError(f"Expected 234 raster jobs, got {len(jobs)}")
    (temporary / "kit-jobs.json").write_text(json.dumps(jobs, indent=2) + "\n")
    (temporary / "bbox-jobs.json").write_text(json.dumps(bbox_jobs, indent=2) + "\n")
    manifest = {"format": 1, "variants": variants, "expected_exports": {"svg": 338, "png": 234, "ico": 13, "total": 585},
                "provenance": "Reconstructed equivalent; the transcripts omit initial source and the full original filename list.",
                "selected_variant": "D4", "raster_jobs": jobs}
    (ROOT / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    (ROOT / "tokens.css").write_text(":root {\n" + "".join(f"  --hm-{name}: {value};\n" for name, value in {
        "ink": INK, "panel": "#0B111A", "panel-raised": "#111A26", "line": LINE,
        "mint": MINT, "cyan": CYAN, "foreground": FG, "muted": MUTED,
        "paper": PAPER, "evergreen": EVERGREEN, "blue": BLUE,
    }.items()) + "}\n")
    print(f"svg written; png jobs: {len(jobs)}; variants: {len(variants)}")


if __name__ == "__main__":
    main()
