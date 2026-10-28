# Headmesh transcript reconstruction

This folder preserves all supplied commands and reconstructs all 13 candidates: A1–A3 Fullmesh, B1–B3 Head node, C1–C3 Weave, and D1–D4 Arch. D4 Shared Leg is the selected identity. Open `index.html` for the offline browser, or `preview/kit.png` for the complete contact sheet.

## Complete PNG collection

Open [`png/index.html`](png/index.html) to browse and download **356 PNGs**, one per SVG: all 338 candidate
assets, the type-study sheet, fourteen selected-brand assets, and three live-site logo/favicon assets. Files in
`png/` are grouped by family and variant, with `selected-brand/`, `site/`, and `previews/` for the extras.
Every PNG preserves its source SVG's dimensions and transparency; scalable site marks without explicit sizes
are exported at 512 px wide. The earlier 234 size-specific PNGs remain under `out/headmesh-brand/`.

Selected icons include rounded squares, circles, and sharp squares in both light and dark themes. Browse
[`png/selected-brand/icons/index.html`](png/selected-brand/icons/index.html) for paired SVG/PNG downloads.
The light theme intentionally uses evergreen-to-blue (`#0A7A5C` → `#0A70B0`) on paper; the dark theme uses
mint-to-cyan (`#3CEFB4` → `#27BFF5`) on ink. The shared leg is fully opaque in both themes.

`source-extras/` preserves the exact SVGs copied from the selected public kit and site. Its manifest records
their checkout paths and SHA-256 hashes. The renderer embeds the prepared brand fonts for the few SVGs that
contain live text; exported typography does not depend on installed system fonts.

`png/validation.json` records source hashes, dimensions, alpha ranges, painted bounds, and checks against
previous same-size renders. `png/SHA256SUMS` covers every new PNG. All 356 exports are shown on the 14 sheets
under `png/contact-sheets/`, including the D4 sheet at `png/contact-sheets/D4.jpg`.

To regenerate the PNG collection alone, run `npm run png`. Inside the full repository, refresh the copied
site assets and regenerate with `.venv/bin/python gen/png_exports.py build --sync-site`.

## What came from the transcripts

Four unique transcripts contain 44 shell command blocks. The two final attachments were identical. `sources/inventory.json` records every input hash, source line, duplicate, created-file event, command, and recovered function. Historical shell blocks are saved under `sources/commands/`; they are reference material. The runnable local build is the npm script below.

- `mark_A1` (final hub revision) and `mark_D4` were recovered as complete functions. Their syntax trees are checked against the saved snippets.
- The `constellation` function is recovered code, including the final grant-label placement. Its random seeds, tenant isolation, and recorded OG/banner placement formulas are used.
- The recorded 512px bounding boxes exist for all 13 marks. The build checks the reconstructed marks against those boxes.
- The Bricolage wordmark uses weight 800, optical size 96, width 100, and tracking -2.5%. Its bounds are checked against the recorded `[3.6, -70.0, 449.0, 1.4]` at 100px. Font instances and their licenses are included; `fonts/manifest.json` records their origins and hashes.
- The ten type studies reproduce the recorded `wm_test.py` text/font/tracking combinations. The supplied raster originals and both homepage themes are preserved byte-for-byte under `references/`.

## Reconstructed portions

The transcripts say “Created a file” for the initial `textpath.py`, `marks.py`, `preview.py`, `render.js`, `build.py`, and `global.css` without including their contents. Commands that edit those files do not recover every original function. The remaining mark bases, text outlining helper, renderer, and export layout here are reconstructed from the available edits, bounding boxes, and images. The earlier A1 board predates its final hub revision.

The transcript reports **585 assets and 234 raster jobs** but does not list the entire export schema. This build supplies a documented equivalent: **26 standalone SVGs, 18 PNGs, and one three-frame ICO per candidate**. The extra monochrome formats, layout choices, and some filenames are inferred. Matching the original counts or bounding boxes does not establish pixel equality with missing original artwork.

All exported SVG typography is outlined and requires no installed fonts. Dark/light lockups, transparent marks, wordmarks, stacked lockups, ink/gradient/paper app icons, favicons, OG cards, X banners, and LinkedIn banners are included. SVG and PNG social artwork are built from the same vector source. The original downloaded banners remain available separately, including the earlier open-constellation X artwork and the C1 LinkedIn candidate.

## Rebuild

From this directory, with Node and Python installed:

```bash
npm ci --ignore-scripts
npx playwright install chromium
python3 -m venv .venv
.venv/bin/python -m pip install -r requirements.txt
.venv/bin/python gen/prepare_fonts.py
npm run build
```

The prepared TTFs are included, so rebuilding artwork alone needs only the pinned Python dependencies and Chromium. `prepare_fonts.py` recreates the font instances from pinned Fontsource packages and the recorded Google Fonts revision; it preserves font licenses.

The build writes `out/headmesh-brand/`, browser previews, `validation.json`, `SHA256SUMS`, and the complete
`png/` collection. `manifest.json` lists every variant and legacy PNG dimension; `png/manifest.json` maps every
SVG to its complete PNG export. Verification checks SVG references, outlined type, nonempty raster exports,
image dimensions, recorded bounding boxes, recovered function identity, wordmark metrics, ICO frame pixels,
and preserved reference hashes. The source transcripts and previous kit remain available in the workspace.

The offline gallery is also checked for broken images, family-filter behavior, and horizontal overflow at 390px. Horizontal lockup checks verify that transformed masks preserve each mark's visible bounds.

To create a source-and-artwork ZIP after a successful build:

```bash
.venv/bin/python gen/package_kit.py
```

The package includes the generators, pinned dependency files, prepared fonts and licenses, transcripts and
command inventory, original image references, previews, all 585 legacy exports, and all 356 additional PNGs
with their source SVG copies, catalog, and contact sheets. Installed dependencies and temporary render probes
are excluded. The packager verifies all 941 exported asset hashes inside the ZIP and writes an archive checksum.
