"""Create and inspect one native-size PNG for every SVG in the complete brand kit."""

import argparse
import hashlib
import html
import json
import math
import re
import shutil
import subprocess
import textwrap
import xml.etree.ElementTree as ET
from collections import Counter
from pathlib import Path

from PIL import Image, ImageChops, ImageDraw, ImageFont, ImageStat

ROOT = Path(__file__).resolve().parents[1]
PNG = ROOT / "png"
MARKETING = ROOT.parents[1]
EXTRAS = ROOT / "source-extras"


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def sources():
    entries = []
    for source in sorted((ROOT / "out/headmesh-brand").rglob("*.svg")):
        relative = source.relative_to(ROOT / "out/headmesh-brand")
        output = Path(*[part for part in relative.parts if part != "svg"]).with_suffix(".png")
        entries.append((source, output, relative.parts[1].split("-")[0]))
    for source in sorted((ROOT / "preview").glob("*.svg")):
        entries.append((source, Path("previews") / source.with_suffix(".png").name, "Extras"))
    for source in sorted(EXTRAS.rglob("*.svg")):
        entries.append((source, source.relative_to(EXTRAS).with_suffix(".png"), "Extras"))
    return entries


def dimensions(source):
    svg = ET.fromstring(source.read_text())
    viewbox = [float(value) for value in re.split(r"[\s,]+", svg.attrib["viewBox"].strip())]
    width = svg.get("width")
    height = svg.get("height")
    # The live site's scalable marks specify only a viewBox. Export those at 512 px wide.
    if width is None and height is None:
        return 512, round(512 * viewbox[3] / viewbox[2])
    if width is None or height is None:
        raise ValueError(f"Incomplete SVG dimensions: {source}")
    if not re.fullmatch(r"\d+(?:\.\d+)?(?:px)?", width) or not re.fullmatch(r"\d+(?:\.\d+)?(?:px)?", height):
        raise ValueError(f"Unsupported SVG dimensions: {source}")
    return math.ceil(float(width.removesuffix("px"))), math.ceil(float(height.removesuffix("px")))


def sync_site():
    public = MARKETING / "public/brand/headmesh-v1"
    originals = [(source, Path("selected-brand") / source.relative_to(public)) for source in sorted(public.rglob("*.svg"))]
    originals += [(MARKETING / "src/assets" / name, Path("site") / name) for name in ("logo.svg", "logo-on-light.svg")]
    originals += [(MARKETING / "public/favicon.svg", Path("site/favicon.svg"))]
    originals += [(public / "logos/manifest.json", Path("selected-brand/logos/manifest.json"))]
    records = []
    for source, relative in originals:
        destination = EXTRAS / relative
        destination.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source, destination)
        records.append({"file": str(relative), "source": str(source.relative_to(MARKETING)), "sha256": sha(source)})
    (EXTRAS / "manifest.json").write_text(json.dumps(records, indent=2) + "\n")


def prepare():
    PNG.mkdir(exist_ok=True)
    entries = []
    jobs = []
    for source, relative, group in sources():
        width, height = dimensions(source)
        target = PNG / relative
        background = ET.fromstring(source.read_text()).get("data-background", "transparent")
        entries.append({"svg": str(source.relative_to(ROOT)), "png": str(relative), "group": group,
                        "width": width, "height": height, "background": background, "svg_sha256": sha(source)})
        jobs.append({"in": str(source.relative_to(ROOT)), "out": str(target.relative_to(ROOT)), "w": width, "h": height})
    if len({entry["png"] for entry in entries}) != len(entries):
        raise ValueError("Colliding PNG filenames")
    (PNG / "manifest.json").write_text(json.dumps({"format": 1, "count": len(entries), "entries": entries}, indent=2) + "\n")
    temporary = ROOT / "tmp"
    temporary.mkdir(exist_ok=True)
    (temporary / "all-svg-png-jobs.json").write_text(json.dumps(jobs, indent=2) + "\n")
    print(f"Prepared {len(entries)} SVG-to-PNG exports", flush=True)


def font(size):
    return ImageFont.truetype(str(ROOT / "fonts/GeistMono-500.ttf"), size)


def sheet(entries, name):
    columns, cell_width, cell_height = 5, 300, 210
    board = Image.new("RGB", (columns * cell_width, math.ceil(len(entries) / columns) * cell_height), "#252d38")
    draw = ImageDraw.Draw(board)
    for index, entry in enumerate(entries):
        image = Image.open(PNG / entry["png"]).convert("RGBA")
        # A dark checker for white/mint marks; a light checker for ink marks. Opaque artwork carries its own background.
        mean = ImageStat.Stat(image, image.getchannel("A")).mean
        dark = sum(mean[:3]) / 3 > 128
        colors = ("#070b12", "#111a26") if dark else ("#f4f7f6", "#e9efee")
        left, top = (index % columns) * cell_width, (index // columns) * cell_height
        frame = Image.new("RGBA", (cell_width - 16, cell_height - 68), colors[0])
        pattern = ImageDraw.Draw(frame)
        for y in range(0, frame.height, 16):
            for x in range(0, frame.width, 16):
                if (x // 16 + y // 16) % 2:
                    pattern.rectangle((x, y, x + 15, y + 15), fill=colors[1])
        image.thumbnail((frame.width - 20, frame.height - 20), Image.Resampling.LANCZOS)
        frame.alpha_composite(image, ((frame.width - image.width) // 2, (frame.height - image.height) // 2))
        board.paste(frame.convert("RGB"), (left + 8, top + 8))
        label = Path(entry["png"]).stem
        for row, line in enumerate(textwrap.wrap(label, width=35)):
            draw.text((left + 10, top + cell_height - 56 + row * 14), line, font=font(12), fill="#f2f6f9")
        draw.text((left + 10, top + cell_height - 21), f"{entry['width']} x {entry['height']}", font=font(11), fill="#9aa8bc")
    directory = PNG / "contact-sheets"
    directory.mkdir(exist_ok=True)
    board.save(directory / f"{name}.jpg", quality=92, subsampling=0)


def icon_gallery(entries):
    directory = PNG / "selected-brand/icons"
    icons = {Path(entry["png"]).stem: entry for entry in entries if Path(entry["png"]).parent == Path("selected-brand/icons")}
    variants = [
        ("Rounded square", "Dark", "app-icon"),
        ("Rounded square", "Light", "app-icon-on-light"),
        ("Rounded square", "Gradient", "app-icon-on-gradient"),
        ("Circle", "Dark", "app-icon-circle-on-dark"),
        ("Circle", "Light", "app-icon-circle-on-light"),
        ("Circle", "Gradient", "app-icon-circle-on-gradient"),
        ("Square", "Dark", "app-icon-square-on-dark"),
        ("Square", "Light", "app-icon-square-on-light"),
        ("Square", "Gradient", "app-icon-square-on-gradient"),
    ]
    cards = []
    for shape, theme, name in variants:
        entry = icons[name]
        svg = "../../../" + entry["svg"]
        cards.append(f'<article><h2>{shape} · {theme}</h2><div class="image"><img src="{name}.png" width="512" height="512" alt="Headmesh {shape.lower()} icon, {theme.lower()} theme"></div><p><a href="{name}.png" download>PNG · 512 × 512</a> · <a href="{svg}" download>SVG</a></p></article>')
    (directory / "index.html").write_text('''<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Headmesh icon shapes</title><style>
    :root{color-scheme:dark}body{margin:0;background:#070b12;color:#f2f6f9;font:16px/1.5 system-ui}main{max-width:1200px;margin:auto;padding:24px}h1{font-size:32px}h2{font-size:18px}p{color:#9aa8bc}a{color:#3cefb4}.grid{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:24px}article{min-width:0;border:1px solid #1c2736;border-radius:12px;padding:20px}.image{background:repeating-conic-gradient(#1c2736 0% 25%,#111a26 0% 50%) 0 0/24px 24px;padding:20px}img{display:block;width:100%;height:auto;border-radius:0}article p{margin-bottom:0}@media(max-width:700px){.grid{grid-template-columns:1fr}}
    </style></head><body><main><h1>Headmesh · icon shapes</h1><p>Rounded square, circle, and sharp square. Every shape uses the same shared-leg geometry and padding on dark, light, and gradient backgrounds.</p><p>The light theme intentionally uses deeper evergreen-to-blue; the dark theme uses mint-to-cyan. Both shared-leg gradients are fully opaque. Gradient backgrounds use a solid black mark. Checkerboards show transparency outside the rounded and circular backgrounds.</p><p><a href="../../index.html">All PNG assets</a> · <a href="../logos/index.html">Logo system</a></p><div class="grid">''' + ''.join(cards) + '</div></main></body></html>\n')


def logo_gallery(entries):
    directory = PNG / "selected-brand/logos"
    manifest = json.loads((EXTRAS / "selected-brand/logos/manifest.json").read_text())
    exports = {Path(entry["png"]).stem: entry for entry in entries if Path(entry["png"]).parent == Path("selected-brand/logos")}
    cards = []
    for asset in manifest["assets"]:
        entry = exports[asset["name"]]
        stage = "light" if asset["colour"] == "black" or asset["style"] == "on-light" or asset["background"] == "white" else "dark"
        label = html.escape(asset["label"])
        cards.append(f'<article data-layout="{asset["layout"]}" data-colour="{asset["colour"]}" data-background="{asset["background"]}"><h2>{label}</h2><div class="stage {stage}"><img src="{asset["png"]}" width="{entry["width"]}" height="{entry["height"]}" alt="{label}"></div><p><a href="{asset["png"]}" download>PNG · {entry["width"]} × {entry["height"]}</a> · <a href="../../../{entry["svg"]}" download>SVG</a></p></article>')
    (directory / "index.html").write_text('''<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Headmesh logo system</title><style>
    :root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;background:#070b12;color:#f2f6f9;font:16px/1.5 system-ui}main{max-width:1280px;margin:auto;padding:24px}h1{font-size:32px}h2{font-size:16px;margin-top:0}p{color:#9aa8bc}a{color:#3cefb4}.filters{display:flex;gap:16px;flex-wrap:wrap;margin:24px 0}label{display:grid;gap:6px}select{font:inherit;padding:8px;background:#111a26;color:#f2f6f9;border:1px solid #334357;border-radius:8px}.grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:20px}article{min-width:0;border:1px solid #1c2736;border-radius:12px;padding:20px}article[hidden]{display:none}.stage{display:flex;align-items:center;justify-content:center;min-height:240px;padding:16px;border-radius:8px}.dark{background:repeating-conic-gradient(#0b111a 0% 25%,#111a26 0% 50%) 0 0/24px 24px}.light{background:repeating-conic-gradient(#f4f7f6 0% 25%,#e9efee 0% 50%) 0 0/24px 24px}img{display:block;width:100%;max-width:100%;height:208px;object-fit:contain}article p{margin-bottom:0}@media(max-width:640px){.grid{grid-template-columns:1fr}main{padding:16px}.filters{gap:12px}select{max-width:100%}}
    </style></head><body><main><h1>Headmesh · logo system</h1><p>Four layouts, ten colour/background combinations, each in SVG and PNG. The D4 shared-leg geometry and outlined Bricolage wordmark are preserved.</p><p>Checkerboards indicate transparency. Gradient artwork uses deeper green/blue on white and bright mint/cyan on black. Black and white exports use true #000000 and #FFFFFF.</p><p><a href="../../index.html">All PNG assets</a> · <a href="../icons/index.html">Icon shapes</a></p><div class="filters"><label>Layout<select id="layout" aria-label="Layout"><option value="all">All layouts</option><option value="mark">Standalone mark</option><option value="wordmark">Wordmark</option><option value="horizontal">Horizontal lockup</option><option value="vertical">Vertical / stacked lockup</option></select></label><label>Logo colour<select id="colour" aria-label="Logo colour"><option value="all">All colours</option><option value="gradient">Gradient</option><option value="black">Black</option><option value="white">White</option><option value="brand">Shared-leg accent</option></select></label><label>Background<select id="background" aria-label="Background"><option value="all">All backgrounds</option><option value="transparent">Transparent</option><option value="black">Black</option><option value="white">White</option><option value="gradient">Gradient</option></select></label></div><p id="count" aria-live="polite">40 variants</p><div class="grid">''' + ''.join(cards) + '''</div></main><script>const filters=[...document.querySelectorAll('select')];for(const filter of filters)filter.addEventListener('change',()=>{let count=0;for(const card of document.querySelectorAll('article')){card.hidden=!filters.every(filter=>filter.value==='all'||card.dataset[filter.id]===filter.value);if(!card.hidden)count++}document.getElementById('count').textContent=count+(count===1?' variant':' variants')});</script></body></html>\n''')


def gallery(entries):
    cards = []
    for entry in entries:
        path = html.escape(entry["png"], quote=True)
        title = html.escape(entry["png"])
        # Pair transparent artwork with both surfaces so white and ink versions are always visible.
        cards.append(f'<article data-group="{entry["group"]}"><h2>{title}</h2><a href="{path}"><div class="pair"><img loading="lazy" class="dark" src="{path}" alt="{title} on dark"><img loading="lazy" class="light" src="{path}" alt="{title} on light"></div></a><p>{entry["width"]} × {entry["height"]} · <a href="{path}" download>Download PNG</a></p></article>')
    groups = list(dict.fromkeys(entry["group"] for entry in entries))
    options = '<option value="all">All PNGs</option>' + ''.join(f'<option>{group}</option>' for group in groups)
    content = f'''<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Headmesh PNG assets</title><style>
    :root{{color-scheme:dark}}body{{margin:0;background:#070b12;color:#f2f6f9;font:15px/1.5 system-ui}}main{{max-width:1200px;margin:auto;padding:24px}}a{{color:#3cefb4}}h1{{font-size:32px}}h2{{font:13px/1.5 monospace;overflow-wrap:anywhere}}.intro,p{{color:#9aa8bc}}select{{padding:10px;background:#111a26;color:#f2f6f9;border:1px solid #1c2736;border-radius:8px}}article{{border-bottom:1px solid #1c2736;padding:16px 0}}article[hidden]{{display:none}}.pair{{display:grid;grid-template-columns:1fr 1fr;gap:12px}}img{{width:100%;height:180px;object-fit:contain;box-sizing:border-box;padding:16px;border-radius:10px}}.dark{{background:repeating-conic-gradient(#0b111a 0% 25%,#111a26 0% 50%) 0 0/24px 24px}}.light{{background:repeating-conic-gradient(#f4f7f6 0% 25%,#e9efee 0% 50%) 0 0/24px 24px}}@media(max-width:500px){{.pair{{grid-template-columns:1fr}}}}
    </style></head><body><main><h1>Headmesh PNG assets</h1><p class="intro">{len(entries)} PNGs, one per SVG. Original proportions and transparency are preserved. SVGs without explicit dimensions use a 512 px export width. The legacy 234 size-specific PNGs are also retained in ../out/headmesh-brand/.</p><p><a href="../index.html">Brand gallery</a> · <a href="manifest.json">Source map and dimensions</a> · <a href="validation.json">Export checks</a> · <a href="contact-sheets/D4.jpg">D4 Shared Leg contact sheet</a></p><label for="group">Show assets: </label><select id="group">{options}</select>{''.join(cards)}</main><script>document.querySelector('select').addEventListener('change',event=>{{document.querySelectorAll('[data-group]').forEach(card=>card.hidden=event.target.value!=='all'&&card.dataset.group!==event.target.value)}});</script></body></html>'''
    content = content.replace('<a href="../index.html">Brand gallery</a>', '<a href="selected-brand/logos/index.html">Logo system · SVG + PNG</a> · <a href="selected-brand/icons/index.html">Icon shapes · SVG + PNG</a> · <a href="../index.html">Brand gallery</a>', 1)
    (PNG / "index.html").write_text(content)
    icon_gallery(entries)
    logo_gallery(entries)


def verify():
    manifest = json.loads((PNG / "manifest.json").read_text())
    entries = manifest["entries"]
    issues, results = [], []
    actual = {str(relative) for _, relative, _ in sources()}
    if actual != {entry["png"] for entry in entries}:
        issues.append("PNG manifest does not cover every current SVG")
    if {str(path.relative_to(PNG)) for path in PNG.rglob("*.png")} != actual:
        issues.append("PNG files do not match the SVG inventory")
    original_jobs = json.loads((ROOT / "manifest.json").read_text())["raster_jobs"]
    original_by_svg = {job["in"]: job for job in original_jobs if dimensions(ROOT / job["in"]) == (job["w"], job["h"])}
    matches = 0
    for entry in entries:
        source, target = ROOT / entry["svg"], PNG / entry["png"]
        if sha(source) != entry["svg_sha256"]:
            issues.append(f"SVG changed after inventory: {entry['svg']}")
        with Image.open(target) as png:
            png.verify()
        with Image.open(target) as png:
            if png.format != "PNG" or png.size != (entry["width"], entry["height"]):
                issues.append(f"Wrong PNG format or dimensions: {entry['png']}")
            rgba = png.convert("RGBA")
            alpha = rgba.getchannel("A")
            bounds = alpha.getbbox()
            if not bounds or not ImageChops.difference(rgba, Image.new("RGBA", rgba.size, (0, 0, 0, 0))).getbbox():
                issues.append(f"Empty PNG: {entry['png']}")
            if entry.get("background", "transparent") == "transparent" and alpha.getextrema()[0] == 255 and any(token in target.stem for token in ("mark-", "wordmark-", "lockup-")):
                issues.append(f"Transparent artwork flattened: {entry['png']}")
            prior = original_by_svg.get(entry["svg"])
            if prior:
                with Image.open(ROOT / prior["out"]) as previous:
                    if rgba.tobytes() != previous.convert("RGBA").tobytes():
                        issues.append(f"PNG differs from the existing same-size render: {entry['png']}")
                    else:
                        matches += 1
            results.append({**entry, "png_sha256": sha(target), "alpha_range": alpha.getextrema(), "paint_bounds": bounds})
    groups = list(dict.fromkeys(entry["group"] for entry in entries))
    for group in groups:
        sheet([entry for entry in entries if entry["group"] == group], group)
    gallery(entries)
    report = {"passed": not issues, "issues": issues, "pngs": len(entries),
              "groups": dict(Counter(entry["group"] for entry in entries)),
              "source_and_dimension_checks": len(entries), "existing_identical_renders": matches,
              "contact_sheets": len(groups), "entries": results}
    (PNG / "validation.json").write_text(json.dumps(report, indent=2) + "\n")
    (PNG / "SHA256SUMS").write_text(''.join(f"{entry['png_sha256']}  {entry['png']}\n" for entry in results))
    print(json.dumps({key: value for key, value in report.items() if key != "entries"}, indent=2), flush=True)
    if issues:
        raise SystemExit(1)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("build", "prepare", "verify"))
    parser.add_argument("--sync-site", action="store_true", help="Refresh the selected-brand and site SVG copies from this checkout before rendering")
    args = parser.parse_args()
    if args.sync_site:
        sync_site()
    if args.action in ("prepare", "build"):
        prepare()
    if args.action == "build":
        subprocess.run(["node", "gen/render.js", "--batch", "tmp/all-svg-png-jobs.json"], cwd=ROOT, check=True)
    if args.action in ("verify", "build"):
        verify()


if __name__ == "__main__":
    main()
