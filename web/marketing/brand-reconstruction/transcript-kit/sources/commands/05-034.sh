python3 gen/build.py && time node gen/render.js --batch tmp/kit-jobs.json && python3 - <<'EOF'
import glob, os
from PIL import Image
for vdir in sorted(glob.glob('out/headmesh-brand/*/*/')):
    ims=[Image.open(os.path.join(vdir,'png',f'favicon-{s}.png')).convert('RGBA') for s in (16,32,48)]
    ims[2].save(os.path.join(vdir,'favicon.ico'), sizes=[(16,16),(32,32),(48,48)], append_images=ims[:2])
print(len(glob.glob('out/headmesh-brand/*/*/favicon.ico')), 'ico files')
EOF
du -sh out/headmesh-brand; find out/headmesh-brand -type f | wc -l
