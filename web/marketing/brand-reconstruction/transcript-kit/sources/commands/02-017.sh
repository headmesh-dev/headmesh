(nohup pnpm preview --port 4321 > /tmp/claude-0/-home-claude/743d91fe-3243-5280-bc7c-5f0209889ee4/scratchpad/preview.log 2>&1 &) ; sleep 6; curl -s -o /dev/null -w "%{http_code}\n" http://localhost:4321/; cat > /tmp/claude-0/-home-claude/743d91fe-3243-5280-bc7c-5f0209889ee4/scratchpad/shoot_hm.js <<'EOF'
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
EOF
mkdir -p /home/claude/research/hm && node /tmp/claude-0/-home-claude/743d91fe-3243-5280-bc7c-5f0209889ee4/scratchpad/shoot_hm.js http://localhost:4321 /home/claude/research/hm before && cd /home/claude/research/hm && python3 -c "
from PIL import Image; import glob
for f in sorted(glob.glob('before-*.png')):
    im=Image.open(f); print(f, im.size)
    w,h=im.size
    if w>1000: im.convert('RGB').resize((w//2,h//2)).save(f.replace('.png','.jpg'),quality=72)
    else: im.convert('RGB').save(f.replace('.png','.jpg'),quality=72)
"
