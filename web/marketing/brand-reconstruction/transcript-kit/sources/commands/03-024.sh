cd /home/claude/brand/gen && python3 - <<'EOF'
s=open('build.py').read()
s=s.replace("mx_, my_ = (left[0] + right[0]) / 2, (left[1] + right[1]) / 2 - fs * 1.5",
            "mx_ = x + wa + gap / 2\n        my_ = left[1] + (right[1] - left[1]) * ((mx_ - left[0]) / (right[0] - left[0])) - fs * 1.4")
open('build.py','w').write(s)
EOF
cd /home/claude/brand && python3 gen/build.py && node gen/render.js --batch tmp/kit-jobs.json && python3 - <<'EOF'
import glob, os
from PIL import Image
for vdir in sorted(glob.glob('out/headmesh-brand/*/*/')):
    ims=[Image.open(os.path.join(vdir,'png',f'favicon-{s}.png')).convert('RGBA') for s in (16,32,48)]
    ims[2].save(os.path.join(vdir,'favicon.ico'), sizes=[(16,16),(32,32),(48,48)], append_images=ims[:2])
print('ok')
EOF
