cd /home/claude/research/hm && sed -i "s/waitUntil: 'networkidle'/waitUntil: 'load', timeout: 45000/" /tmp/claude-0/-home-claude/743d91fe-3243-5280-bc7c-5f0209889ee4/scratchpad/shoot_hm.js && node /tmp/claude-0/-home-claude/743d91fe-3243-5280-bc7c-5f0209889ee4/scratchpad/shoot_hm.js http://localhost:4321 /home/claude/research/hm before && python3 -c "
from PIL import Image; import glob
for f in sorted(glob.glob('before-*.png')):
    im=Image.open(f); print(f, im.size)
    w,h=im.size
    im=im.convert('RGB')
    (im.resize((w//2,h//2)) if w>1000 else im).save(f.replace('.png','.jpg'),quality=72)
"
