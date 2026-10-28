# Headmesh logo system

Open [index.html](index.html) to compare and download all 40 SVG/PNG pairs. Typography is outlined; no fonts are required.

| Layout | Filename prefix | PNG canvas |
| --- | --- | --- |
| Standalone mark | mark- | 512 × 512 |
| Wordmark | wordmark- | 1000 × 240 |
| Horizontal lockup | lockup-horizontal- | 1200 × 280 |
| Vertical / stacked lockup | lockup-vertical- | 720 × 576 |

Each layout has these ten variants:

- **gradient**: Gradient logo · transparent.
- **black**: Black logo · transparent.
- **white**: White logo · transparent.
- **on-dark**: Shared-leg accent · for dark surfaces.
- **on-light**: Shared-leg accent · for light surfaces.
- **white-on-black**: White logo · black background.
- **black-on-white**: Black logo · white background.
- **gradient-on-black**: Gradient logo · black background.
- **gradient-on-white**: Gradient logo · white background.
- **black-on-gradient**: Black logo · gradient background.

## Colour and background rules

- Black and white artwork/backgrounds use true #000000 and #FFFFFF.
- The full-colour gradient covers the whole mark and wordmark. It uses mint-to-cyan (#3CEFB4 → #27BFF5) for dark surfaces; gradient-on-white uses deeper evergreen-to-blue (#0A7A5C → #0A70B0) for contrast.
- on-dark and on-light preserve the original shared-leg accent: white/ink outer shapes with colour only on the shared leg. Their backgrounds are transparent. The original mark aliases retain their tight crop; other exports include consistent canvas padding.
- black-on-gradient has an opaque mint-to-cyan background and a solid black logo. Background colours are included in both SVG and PNG files.
- Transparent exports preserve alpha. Use white artwork on dark surfaces and black artwork on light surfaces.
- The two original lockup-on-dark/light SVGs remain available as compatibility exports. New lockup-horizontal-* filenames identify the complete current set.

manifest.json records every current logo's layout, colour, background, filename, and native export size.
