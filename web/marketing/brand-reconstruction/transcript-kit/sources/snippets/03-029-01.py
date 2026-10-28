import sys; sys.path.insert(0,'gen')
from textpath import text_paths, fmt
tp=text_paths("headmesh","BricolageGrotesque-800.ttf",100,tracking=-0.025)
x0,y0,x1,y1=tp['bounds']; print('bounds',[round(v,2) for v in tp['bounds']])
# normalise to origin
tp=text_paths("headmesh","BricolageGrotesque-800.ttf",100,tracking=-0.025,x0=-x0,baseline=-y0)
d=fmt(tp['d'],1)
print('w',round(x1-x0,2),'h',round(y1-y0,2),'baseline',round(-y0,2), 'len',len(d))
open('tmp/wordmark_d.txt','w').write(d)
