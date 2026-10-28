import sys, json
sys.path.insert(0,'gen')
from marks import MARKS, icon_svg
jobs=[]
for k in MARKS:
    p=f'tmp/bbox/{k}.svg'; open(p,'w').write(icon_svg(k,'#000000',pid=k.lower()))
    jobs.append({"in":p,"out":f'tmp/bbox/{k}.png',"w":512,"h":512,"scale":1})
json.dump(jobs,open('tmp/bbox/jobs.json','w'))
