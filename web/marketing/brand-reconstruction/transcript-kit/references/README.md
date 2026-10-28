# Headmesh original design references

The original `hm1`–`hm9` downloads have descriptive filenames here. All files retain their original image bytes and dimensions; the table maps each saved name back to its download name and the transcript.

| Saved filename | Original download | Transcript match / contents |
| --- | --- | --- |
| `banner-x-1500x500.png` | `hm1.png` | 1500 × 500 dark banner; matches the transcript's X banner export size and D4 shared-leg mark. |
| `before-home-desk-dark.jpg` | `hm2.jpeg` | Dark homepage reference. The transcript captured `before-home-desk-dark.png` at 1440 × 3508 and then saved a half-size JPEG; this download is 720 × 1754. |
| `before-home-desk-light.jpg` | `hm3.jpeg` | Light homepage reference; same capture/export sequence as the dark page. |
| `og-card-flat-1200x630.png` | `hm4.png` | 1200 × 630 D4 brand card with the headline and network motif. |
| `logo-system-study-1.jpeg` | `hm5.jpeg` | Logo-system exploration board; includes the D4 shared-leg candidate and other marks. |
| `banner-linkedin-c1-1584x396.png` | `hm6.png` | 1584 × 396 banner candidate; matches the transcript's LinkedIn export size. It shows the C1 weave mark. |
| `wordmark-type-study.png` | `hm7.png` | Wordmark type study. The transcript selects Bricolage Grotesque ExtraBold (800) with tracking `-0.025` for the final lowercase wordmark. |
| `og-card-grant-1200x630.png` | `hm8.png` | 1200 × 630 D4 card with the two-tenant, temporary-grant diagram; matches the later OG-card design in the transcript. |
| `logo-system-study-2.jpeg` | `hm9.jpeg` | Additional logo-system exploration board, showing the A/B/C mark candidates. |

The transcript's generator produced the full D4 mark geometry, dark/light theme gradient stops, and Bricolage wordmark settings. The raster downloads are visual references and final-size art; they are not substitutes for the original 585-file vector export from the isolated Claude workspace.

The current marketing site was captured after the branding changes as `after-home-desk-dark.png` and `after-home-desk-light.png` at 1440 × 3600. These are full-size Playwright renders; the supplied `before-home-desk-*` references are 720 × 1754 half-size JPEGs.

The later `synced-*` captures show the shared marketing/docs theme from `src/styles/brand.css`: the homepage
and docs overview in both themes at 1440 px, the mobile quickstart in both themes at 390 px, the dark mobile
waitlist, and the light desktop privacy page. These are local browser renders, not production captures or form
submission results. The earlier `before-*` and `after-*` images are retained for comparison.
