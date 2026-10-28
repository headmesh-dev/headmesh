# Headmesh brand kit — D4 Shared Leg

This kit follows the selected D4 “Arch · Shared Leg” mark and final wordmark recorded in the Claude Design transcript. The mark uses the transcript's exact path geometry and gradient axis. The wordmark uses the outlined lowercase Bricolage Grotesque ExtraBold path (weight 800, tracking -2.5%) from src/components/Wordmark.astro.

## Contents

- logos/: D4 marks and horizontal lockups for dark and light surfaces.
- icons/: app icons, favicons, Apple touch icon, and ICO bundle.
- social/: editable SVGs and PNGs. The X banner and home OG card are byte-for-byte copies of banner-x-1500x500.png and og-card-grant-1200x630.png in brand-reference/headmesh-process/. The D4 LinkedIn banner is a local vector reconstruction using the selected mark, outlined wordmark, and bundled fonts; the supplied C1 LinkedIn exploration is preserved separately as a candidate.
- tokens.css: dark and light color custom properties from the transcript.
- preview.html: local visual contact sheet.

The descriptive filenames in brand-reference/headmesh-process/ map to the original hm1–hm9 download names in that directory's README. The full 585-file export from the isolated Claude workspace was not available; this repo contains the original downloaded references and reconstructed vector assets.

## Regenerate

From web/marketing/:

    pnpm exec node scripts/generate-brand-kit.mjs

The generator reads the outlined wordmark component and transcript reference images, then writes the versioned kit under public/brand/headmesh-v1/.
