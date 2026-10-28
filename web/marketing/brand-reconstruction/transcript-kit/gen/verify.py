"""Check exported assets and recovered code; build the original three-size ICO bundles."""

import ast
import hashlib
import inspect
import json
import math
import re
import xml.etree.ElementTree as ET
from collections import Counter
from pathlib import Path

from PIL import Image

import marks
import motif
from textpath import text_paths

ROOT = Path(__file__).resolve().parents[1]


def function_ast(source, name):
    tree = ast.parse(source)
    return ast.dump(next(node for node in tree.body if isinstance(node, ast.FunctionDef) and node.name == name), include_attributes=False)


def main():
    manifest = json.loads((ROOT / "manifest.json").read_text())
    issues, boxes, placements = [], {}, {}
    for variant in manifest["variants"]:
        directory = ROOT / variant["directory"]
        images = [Image.open(directory / "png" / f"favicon-{size}.png").convert("RGBA") for size in (16, 32, 48)]
        images[2].save(directory / "favicon.ico", sizes=[(16, 16), (32, 32), (48, 48)], append_images=images[:2])
        with Image.open(directory / "favicon.ico") as ico:
            if ico.ico.sizes() != {(16, 16), (32, 32), (48, 48)}:
                issues.append(f"Wrong ICO frame sizes: {variant['key']}")
            for size, image in zip((16, 32, 48), images):
                if ico.ico.getimage((size, size)).convert("RGBA").tobytes() != image.tobytes():
                    issues.append(f"ICO frame differs from source PNG: {variant['key']} {size}")
        with Image.open(ROOT / f"tmp/bbox-{variant['key']}.png") as image:
            actual = list(image.convert("RGBA").getchannel("A").getbbox())
            expected = variant["recorded_bbox"]
            boxes[variant["key"]] = {"actual": actual, "recorded": expected, "matches": actual == expected}
            if actual != expected:
                issues.append(f"Recorded bbox differs: {variant['key']}: {actual} != {expected}")
        svg_root = ET.fromstring((directory / "svg/lockup-horizontal-on-dark.svg").read_text())
        svg_width, svg_height = float(svg_root.attrib["width"]), float(svg_root.attrib["height"])
        with Image.open(directory / "png/lockup-horizontal-on-dark-1200.png") as lockup:
            scale = min(lockup.width / svg_width, lockup.height / svg_height)
            offset_x, offset_y = (lockup.width - svg_width * scale) / 2, (lockup.height - svg_height * scale) / 2
            actual = lockup.convert("RGBA").crop((0, 0, int(offset_x + 110 * scale), lockup.height)).getchannel("A").getbbox()
            left, top, right, bottom = variant["recorded_bbox"]
            source_scale = 76 / max(right - left, bottom - top)
            width, height = (right - left) * source_scale, (bottom - top) * source_scale
            expected = [(54 - width / 2) * scale + offset_x, (64 - height / 2) * scale + offset_y,
                        (54 + width / 2) * scale + offset_x, (64 + height / 2) * scale + offset_y]
            expected_pixels = [math.floor(expected[0]), math.floor(expected[1]), math.ceil(expected[2]), math.ceil(expected[3])]
            matches = actual is not None and all(abs(a - b) <= 1 for a, b in zip(actual, expected_pixels))
            placements[variant["key"]] = {"actual": actual, "expected": expected_pixels, "matches": matches}
            if not matches:
                issues.append(f"Placed mark clipped or displaced: {variant['key']}: {actual} != {expected_pixels}")
    for job in manifest["raster_jobs"]:
        with Image.open(ROOT / job["out"]) as image:
            if image.size != (job["w"], job["h"]):
                issues.append(f"Wrong PNG size: {job['out']}")
            if not image.convert("RGBA").getchannel("A").getbbox():
                issues.append(f"Empty PNG: {job['out']}")
    svg_files = sorted((ROOT / "out/headmesh-brand").rglob("*.svg"))
    for svg_path in svg_files:
        source = svg_path.read_text()
        element = ET.fromstring(source)
        ids = [node.attrib["id"] for node in element.iter() if "id" in node.attrib]
        if len(ids) != len(set(ids)):
            issues.append(f"Duplicate SVG ids: {svg_path}")
        if set(re.findall(r"url\(#([^)]+)\)", source)) - set(ids):
            issues.append(f"Unresolved SVG reference: {svg_path}")
        if any(node.tag.rsplit("}", 1)[-1] in ("text", "script", "foreignObject") for node in element.iter()):
            issues.append(f"Non-outlined or active SVG content: {svg_path}")
        if any(name.rsplit("}", 1)[-1] == "href" and not value.startswith("#") for node in element.iter() for name, value in node.attrib.items()):
            issues.append(f"External SVG reference: {svg_path}")
    recovered = {}
    for filename, module, function in [("mark_A1_final.py", marks, "mark_A1"), ("mark_D4.py", marks, "mark_D4"), ("constellation.py", motif, "constellation")]:
        original = (ROOT / "sources/recovered" / filename).read_text()
        implemented = inspect.getsource(getattr(module, function))
        same = function_ast(original, function) == function_ast(implemented, function)
        recovered[function] = same
        if not same:
            issues.append(f"Recovered function changed: {function}")
    bounds = text_paths("headmesh", "BricolageGrotesque-800.ttf", 100, tracking=-0.025)["bounds"]
    if any(abs(actual - target) > 0.001 for actual, target in zip(bounds, (3.6, -70, 449, 1.4))):
        issues.append(f"Wordmark bounds differ from transcript: {bounds}")
    references = json.loads((ROOT / "references/manifest.json").read_text())
    for entry in references:
        if hashlib.sha256((ROOT / "references" / entry["file"]).read_bytes()).hexdigest() != entry["sha256"]:
            issues.append(f"Reference bytes changed: {entry['file']}")
    files = sorted(path for path in (ROOT / "out/headmesh-brand").rglob("*") if path.is_file())
    counts = Counter(path.suffix.lstrip(".") for path in files)
    if counts != Counter({"svg": 338, "png": 234, "ico": 13}):
        issues.append(f"Unexpected export counts: {dict(counts)}")
    for filename in ("kit.png", "marks.png", "wm.png"):
        if not (ROOT / "preview" / filename).is_file():
            issues.append(f"Missing preview: {filename}")
    if (ROOT / "preview/kit.png").exists():
        with Image.open(ROOT / "preview/kit.png") as capture:
            image = capture.convert("RGB")
            width, height = image.size
            image.crop((0, 0, width, height // 2)).save(ROOT / "preview/kit_a.jpg", quality=80)
            image.crop((0, height // 2, width, height)).save(ROOT / "preview/kit_b.jpg", quality=80)
    report = {"passed": not issues, "issues": issues, "exports": dict(counts), "files": len(files),
              "bbox_checks": boxes, "horizontal_placement_checks": placements, "recovered_function_ast_matches": recovered,
              "wordmark_bounds": bounds, "reference_hashes_checked": len(references),
              "scope": "Structural integrity, recorded bounding boxes, code identity, type metrics and reference bytes. These do not establish pixel equality for omitted original source."}
    (ROOT / "validation.json").write_text(json.dumps(report, indent=2) + "\n")
    (ROOT / "SHA256SUMS").write_text(''.join(f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.relative_to(ROOT)}\n" for path in files))
    print(json.dumps(report, indent=2))
    if issues:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
