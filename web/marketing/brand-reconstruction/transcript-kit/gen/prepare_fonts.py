"""Freeze the font instances used by the recorded type studies and export process."""

import hashlib
import json
import shutil
import urllib.parse
import urllib.request
from pathlib import Path

from fontTools.ttLib import TTFont
from fontTools.varLib.instancer import instantiateVariableFont

ROOT = Path(__file__).resolve().parents[1]
FONT_DIR = ROOT / "fonts"
PACKAGES = ROOT / "node_modules"


def fetch(url, destination):
    if not destination.exists():
        request = urllib.request.Request(url, headers={"User-Agent": "Headmesh-brand-reconstruction"})
        with urllib.request.urlopen(request, timeout=45) as response:
            destination.write_bytes(response.read())


def main():
    FONT_DIR.mkdir(parents=True, exist_ok=True)
    upstream = FONT_DIR / "upstream"
    upstream.mkdir(exist_ok=True)
    records = []

    def instance(name, source, axes, origin):
        font = TTFont(source)
        if "fvar" in font:
            selected = {axis.axisTag: axes.get(axis.axisTag, axis.defaultValue) for axis in font["fvar"].axes}
            font = instantiateVariableFont(font, selected, inplace=False)
        font.flavor = None
        font.recalcTimestamp = False
        destination = FONT_DIR / name
        font.save(destination)
        records.append({"file": name, "source": origin, "axes": axes,
                        "sha256": hashlib.sha256(destination.read_bytes()).hexdigest()})

    for family, filename, weights in [
        ("geist", "geist-latin-wght-normal.woff2", (400, 600, 700)),
        ("geist-mono", "geist-mono-latin-wght-normal.woff2", (500,)),
        ("bricolage-grotesque", "bricolage-grotesque-latin-standard-normal.woff2", (700, 800)),
    ]:
        package = PACKAGES / f"@fontsource-variable/{family}"
        source = package / "files" / filename
        shutil.copyfile(package / "LICENSE", FONT_DIR / f"LICENSE-{family}.txt")
        family_name = {"geist": "Geist", "geist-mono": "GeistMono", "bricolage-grotesque": "BricolageGrotesque"}[family]
        for weight in weights:
            axes = {"wght": weight}
            if family == "bricolage-grotesque":
                axes.update({"opsz": 96, "wdth": 100})
            instance(f"{family_name}-{weight}.ttf", source, axes, f"@fontsource-variable/{family}@5.3.0/{filename}")

    revision_file = upstream / "google-fonts-revision.txt"
    if revision_file.exists():
        revision = revision_file.read_text().strip()
    else:
        request = urllib.request.Request("https://api.github.com/repos/google/fonts/commits/main", headers={"User-Agent": "Headmesh-brand-reconstruction"})
        with urllib.request.urlopen(request, timeout=45) as response:
            revision = json.load(response)["sha"]
        revision_file.write_text(revision + "\n")
    for family, filename, label, weights in [
        ("sora", "Sora[wght].ttf", "Sora", (600, 700)),
        ("spacegrotesk", "SpaceGrotesk[wght].ttf", "SpaceGrotesk", (600, 700)),
        ("instrumentsans", "InstrumentSans[wdth,wght].ttf", "InstrumentSans", (700,)),
    ]:
        base = f"https://raw.githubusercontent.com/google/fonts/{revision}/ofl/{family}/"
        source = upstream / filename
        fetch(base + urllib.parse.quote(filename), source)
        fetch(base + "OFL.txt", FONT_DIR / f"LICENSE-{family}.txt")
        for weight in weights:
            instance(f"{label}-{weight}.ttf", source, {"wght": weight, "wdth": 100}, base + urllib.parse.quote(filename))
    (FONT_DIR / "manifest.json").write_text(json.dumps(records, indent=2) + "\n")
    print(f"Prepared {len(records)} pinned font instances, including optical size 96 for Bricolage.")


if __name__ == "__main__":
    main()
