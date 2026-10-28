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
