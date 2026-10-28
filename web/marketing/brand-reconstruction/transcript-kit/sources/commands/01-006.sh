mkdir -p tmp/bbox && python3 - <<'EOF'
import sys, json
sys.path.insert(0,'gen')
from marks import MARKS, icon_svg
jobs=[]
for k in MARKS:
    p=f'tmp/bbox/{k}.svg'; open(p,'w').write(icon_svg(k,'#000000',pid=k.lower()))
    jobs.append({"in":p,"out":f'tmp/bbox/{k}.png',"w":512,"h":512,"scale":1})
json.dump(jobs,open('tmp/bbox/jobs.json','w'))
EOF
node gen/render.js --batch tmp/bbox/jobs.json && python3 - <<'EOF'
import json, sys
from PIL import Image
sys.path.insert(0,'gen')
from marks import MARKS
bb={}
for k in MARKS:
    im=Image.open(f'tmp/bbox/{k}.png').split()[-1]
    bb[k]=im.getbbox()
json.dump(bb,open('gen/bboxes.json','w'),indent=0)
for k,v in bb.items(): print(k,v, 'w',v[2]-v[0],'h',v[3]-v[1], 'cx',(v[0]+v[2])/2,'cy',(v[1]+v[3])/2)
EOF
