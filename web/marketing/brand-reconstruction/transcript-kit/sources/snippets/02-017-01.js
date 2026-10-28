const { chromium } = require('playwright');
const [,, base, outdir, tag] = process.argv;
(async () => {
  const b = await chromium.launch();
  const shots = [
    ['/', 1440, 900, 'dark', 'home-desk-dark'], ['/', 1440, 900, 'light', 'home-desk-light'],
    ['/', 390, 844, 'dark', 'home-mob-dark'], ['/waitlist/', 1440, 900, 'dark', 'waitlist-desk-dark'],
    ['/docs/', 1440, 900, 'dark', 'docs-desk-dark'],
  ];
  for (const [path, w, h, scheme, name] of shots) {
    const ctx = await b.newContext({ viewport: { width: w, height: h }, colorScheme: scheme, deviceScaleFactor: 1 });
    const p = await ctx.newPage();
    await p.goto(base + path, { waitUntil: 'networkidle' });
    await p.waitForTimeout(800);
    await p.screenshot({ path: `${outdir}/${tag}-${name}.png`, fullPage: true });
    await ctx.close();
  }
  await b.close();
})();
