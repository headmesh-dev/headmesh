"""Generate the recorded kit/type-study previews and an offline asset browser."""

import hashlib
import html
import json
import shutil
from pathlib import Path

from marks import CYAN, INK, MARKS, MINT, icon_svg
from textpath import fmt, text_paths

ROOT = Path(__file__).resolve().parents[1]


def main():
    preview = ROOT / "preview"
    preview.mkdir(exist_ok=True)
    manifest = json.loads((ROOT / "manifest.json").read_text())
    rows, cards = [], []
    for variant in manifest["variants"]:
        directory = ROOT / variant["directory"]
        key = variant["key"]
        svg = lambda name: "../" + variant["directory"] + "/svg/" + name
        rows.append(f'''<div class="r"><div class="k">{directory.name}</div>
        <div class="c dark"><img src="{svg('lockup-horizontal-on-dark.svg')}" style="height:70px"></div>
        <div class="c light"><img src="{svg('lockup-horizontal-on-light.svg')}" style="height:70px"></div>
        <img src="{svg('app-icon-ink.svg')}" class="a"><img src="{svg('app-icon-gradient.svg')}" class="a"><img src="{svg('app-icon-paper.svg')}" class="a">
        <div class="c dark"><img src="{svg('lockup-stacked-on-dark.svg')}" style="height:110px"></div>
        <img src="../{variant['directory']}/png/favicon-16.png" style="width:16px;height:16px;image-rendering:pixelated;transform:scale(3);margin:0 24px"></div>''')
        folder = variant["directory"]
        cards.append(f'''<section data-family="{key[0]}"><h2>{key} · {html.escape(variant['label'])}</h2>
        <p class="evidence">{'Recovered mark function' if key in ('A1','D4') else 'Reconstructed base; recorded edits and bounding box'}</p>
        <div class="pair"><div class="sample dark"><img src="{folder}/svg/lockup-horizontal-on-dark.svg" alt="{key} dark lockup"></div><div class="sample light"><img src="{folder}/svg/lockup-horizontal-on-light.svg" alt="{key} light lockup"></div></div>
        <div class="icons"><img src="{folder}/svg/app-icon-ink.svg" alt="Ink app icon"><img src="{folder}/svg/app-icon-gradient.svg" alt="Gradient app icon"><img src="{folder}/svg/app-icon-paper.svg" alt="Paper app icon"></div>
        <img class="banner" src="{folder}/png/banner-linkedin-1584x396.png" alt="{key} reconstructed LinkedIn banner">
        <p><a href="{folder}/svg/og-1200x630.svg">OG SVG</a> · <a href="{folder}/png/og-1200x630.png">OG PNG</a> · <a href="{folder}/png/banner-x-1500x500.png">X banner</a> · <a href="{folder}/favicon.ico">ICO</a></p></section>''')
    kit = '<html><body style="margin:0;background:#2a2f38;font:12px system-ui;color:#ccc"><style>.r{display:flex;align-items:center;gap:12px;padding:8px 12px}.k{width:150px;flex-shrink:0}.c{padding:10px 14px;border-radius:8px}.dark{background:#070B12}.light{background:#F4F7F6}.a{width:72px;height:72px;flex-shrink:0}</style>' + ''.join(rows) + '</body></html>'
    (preview / "kit.html").write_text(kit)

    tests = [("Geist-600.ttf", "headmesh", -0.035, 4), ("Geist-700.ttf", "HeadMesh", -0.03, 4),
             ("Sora-700.ttf", "HeadMesh", -0.02, 4), ("Sora-600.ttf", "headmesh", -0.03, 4),
             ("SpaceGrotesk-600.ttf", "headmesh", -0.02, 4), ("SpaceGrotesk-700.ttf", "HEADMESH", 0.04, 4),
             ("BricolageGrotesque-800.ttf", "headmesh", -0.04, 4), ("BricolageGrotesque-700.ttf", "HeadMesh", -0.03, 4),
             ("InstrumentSans-700.ttf", "headmesh", -0.03, 4), ("GeistMono-500.ttf", "headmesh", -0.02, 4)]
    body = []
    for index, (filename, content, tracking, split) in enumerate(tests):
        y = index * 150
        shape = text_paths(content, filename, 110, tracking=tracking, x0=40, baseline=y + 120, split=split)
        body.append(f'<path fill="#fff" d="{fmt(shape["d_a"])}"/><path fill="url(#g)" d="{fmt(shape["d_b"])}"/><text x="700" y="{y + 110}" fill="#789" font-size="22" font-family="monospace">{filename} {tracking}</text>')
    (preview / "wm.svg").write_text(f'<svg xmlns="http://www.w3.org/2000/svg" width="1100" height="1520" viewBox="0 0 1100 1520"><defs><linearGradient id="g" x1="0" x2="1"><stop offset="0" stop-color="{MINT}"/><stop offset="1" stop-color="{CYAN}"/></linearGradient></defs><rect width="100%" height="100%" fill="{INK}"/>{"".join(body)}</svg>')

    marks = ''.join(f'<article><div>{icon_svg(key, fg="#F2F6F9" if key == "D4" else None)}</div><h2>{key} · {html.escape(label)}</h2><p>{html.escape(family)}</p></article>' for key, (family, label, _) in MARKS.items())
    (preview / "marks.html").write_text('<html><head><style>body{margin:0;padding:40px;background:#070b12;color:#f2f6f9;font:16px system-ui}.grid{display:grid;grid-template-columns:repeat(4,1fr);gap:24px}article{padding:24px;border:1px solid #1c2736;border-radius:20px;text-align:center}article svg{width:220px;height:220px}h2{font-size:20px}p{color:#9aa9b8}</style></head><body><h1>Headmesh · all 13 reconstructed marks</h1><div class="grid">' + marks + '</div></body></html>')
    (ROOT / "tmp/preview-jobs.json").write_text(json.dumps([
        {"in": "preview/kit.html", "out": "preview/kit.png", "w": 1500, "h": 900},
        {"in": "preview/marks.html", "out": "preview/marks.png", "w": 1650, "h": 1200},
        {"in": "preview/wm.svg", "out": "preview/wm.png", "w": 1100, "h": 1520},
    ], indent=2) + "\n")

    reference_source = ROOT.parents[1] / "brand-reference/headmesh-process"
    references = ROOT / "references"
    references.mkdir(exist_ok=True)
    records = []
    for source in sorted(reference_source.iterdir()):
        if source.suffix.lower() in (".png", ".jpg", ".jpeg"):
            target = references / source.name
            shutil.copyfile(source, target)
            records.append({"file": source.name, "sha256": hashlib.sha256(source.read_bytes()).hexdigest()})
    shutil.copyfile(reference_source / "README.md", references / "README.md")
    (references / "manifest.json").write_text(json.dumps(records, indent=2) + "\n")
    (ROOT / "index.html").write_text('''<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Headmesh · transcript reconstruction</title><style>
    :root{color-scheme:dark}body{margin:0;background:#070b12;color:#f2f6f9;font:16px/1.55 system-ui}main{max-width:1200px;margin:auto;padding:32px 24px}h1{font-size:32px;letter-spacing:-.03em}h2{font-size:24px}.intro,.evidence{color:#9aa9b8}.controls{position:sticky;top:0;background:#070b12ee;padding:16px 0;border-bottom:1px solid #1c2736;z-index:2}button,a{color:#3cefb4}button{background:#111a26;border:1px solid #334357;border-radius:8px;padding:9px 16px;margin:4px;cursor:pointer}button[aria-pressed=true]{border-color:#3cefb4}section{margin:32px 0;padding-bottom:28px;border-bottom:1px solid #1c2736}.pair{display:grid;grid-template-columns:1fr 1fr;gap:16px}.sample{padding:28px;border-radius:14px;display:flex;align-items:center;min-height:80px}.sample img{width:100%;height:90px;object-fit:contain}.dark{background:#0b111a}.light{background:#f4f7f6}.icons{display:flex;gap:20px;margin:24px 0}.icons img{width:90px;height:90px}.banner{width:100%;border-radius:12px}section[hidden]{display:none}@media(max-width:650px){.pair{grid-template-columns:1fr}}
    </style></head><body><main><h1>Headmesh · transcript reconstruction</h1><p class="intro">All 13 variants, outlined typography, and the final recorded two-tenant/grant motif. A1 and D4 mark functions and the motif are recovered code. Other bases and the missing export layout are reconstructed from commands and boards. The supplied raster originals are preserved under <a href="references/README.md">references</a>.</p><p><a href="preview/kit.png">Complete contact sheet</a> · <a href="preview/wm.png">Ten wordmark studies</a> · <a href="README.md">Build and provenance</a></p><div class="controls" role="group" aria-label="Filter families"><button data-filter="all" aria-pressed="true">All 13</button><button data-filter="A" aria-pressed="false">A · Fullmesh</button><button data-filter="B" aria-pressed="false">B · Head node</button><button data-filter="C" aria-pressed="false">C · Weave</button><button data-filter="D" aria-pressed="false">D · Arch</button></div>''' + ''.join(cards) + '''<section><h2>Homepage references</h2><p><a href="references/before-home-desk-dark.jpg">Dark before</a> · <a href="references/before-home-desk-light.jpg">Light before</a> · <a href="references/after-home-desk-dark.png">Dark current capture</a> · <a href="references/after-home-desk-light.png">Light current capture</a></p></section></main><script>document.querySelectorAll('[data-filter]').forEach(button=>button.addEventListener('click',()=>{const family=button.dataset.filter;document.querySelectorAll('[data-family]').forEach(section=>section.hidden=family!=='all'&&section.dataset.family!==family);document.querySelectorAll('[data-filter]').forEach(item=>item.setAttribute('aria-pressed',String(item===button)));}));</script></body></html>''')
    browser = ROOT / "index.html"
    browser.write_text(browser.read_text().replace('<a href="preview/kit.png">', '<a href="png/index.html">All SVGs as PNG</a> · <a href="preview/kit.png">', 1))
    print("Prepared contact sheet, marks board, ten type studies, offline browser, and exact reference copies.")


if __name__ == "__main__":
    main()
