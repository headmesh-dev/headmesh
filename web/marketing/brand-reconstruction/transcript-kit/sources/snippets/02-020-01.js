import glob, os
from PIL import Image
for vdir in sorted(glob.glob('out/headmesh-brand/*/*/')):
    ims=[Image.open(os.path.join(vdir,'png',f'favicon-{s}.png')).convert('RGBA') for s in (16,32,48)]
    ims[2].save(os.path.join(vdir,'favicon.ico'), sizes=[(16,16),(32,32),(48,48)], append_images=ims[:2])
print(len(glob.glob('out/headmesh-brand/*/*/favicon.ico')), 'ico files')
