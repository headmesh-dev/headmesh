cd /home/claude/brand/gen && python3 - <<'EOF'
s=open('marks.py').read()
s=s.replace('''MARKS = {''','''def mark_D4(paint="grad", pid="d4", fg=None):
    """Arch · Shared Leg — two h's in the foreground colour; the one leg they share glows."""
    p, g = _paint(paint, pid)
    f = fg or p
    body = (
        f'<g fill="{f}">'
        f'<path d="M88 390V122A34 34 0 0 1 156 122V390A34 34 0 0 1 88 390Z"/>'
        f'<path d="M356 390V122A34 34 0 0 1 424 122V390A34 34 0 0 1 356 390Z"/>'
        f'<path d="M88 280A101 101 0 0 1 290 280H222A33 33 0 0 0 156 280Z"/>'
        f'<path d="M222 280A101 101 0 0 1 424 280H356A33 33 0 0 0 290 280Z"/></g>'
        f'<path fill="{p}" d="M222 280H290V390A34 34 0 0 1 222 390Z"/>'
    )
    return g, body


MARKS = {''')
s=s.replace('''    "D3": ("Arch", "Tunnel", mark_D3),
}''','''    "D3": ("Arch", "Tunnel", mark_D3),
    "D4": ("Arch", "Shared Leg", mark_D4),
}''')
open('marks.py','w').write(s)
EOF
python3 -c "
import sys; sys.path.insert(0,'/home/claude/brand/gen')
from marks import icon_svg
open('/home/claude/brand/tmp/bbox/D4.svg','w').write(icon_svg('D4','#000000'))
"; cd /home/claude/brand && echo '[{"in":"tmp/bbox/D4.svg","out":"tmp/bbox/D4.png","w":512,"h":512}]' > tmp/bbox/j4.json && node gen/render.js --batch tmp/bbox/j4.json && python3 -c "
import json; from PIL import Image
bb=json.load(open('gen/bboxes.json')); bb['D4']=list(Image.open('tmp/bbox/D4.png').split()[-1].getbbox()); json.dump(bb,open('gen/bboxes.json','w')); print(bb['D4'])"
python3 - <<'EOF'
# contrast checks for UI tokens
def lum(h):
    h=h.lstrip('#'); r,g,b=[int(h[i:i+2],16)/255 for i in (0,2,4)]
    f=lambda c: c/12.92 if c<=0.03928 else ((c+0.055)/1.055)**2.4
    return 0.2126*f(r)+0.7152*f(g)+0.0722*f(b)
def cr(a,b):
    la,lb=sorted([lum(a),lum(b)],reverse=True); return (la+0.05)/(lb+0.05)
pairs=[('#3CEFB4','#070B12'),('#27BFF5','#070B12'),('#9AA9B8','#070B12'),('#9AA9B8','#111A26'),('#04170F','#3CEFB4'),
('#0A7A5C','#FFFFFF'),('#0A7A5C','#F3F6F5'),('#087356','#F3F6F5'),('#0A70B0','#FFFFFF'),('#4A5664','#FFFFFF'),('#4A5664','#F3F6F5'),('#FFFFFF','#0A7A5C'),('#0A9E74','#FFFFFF'),('#6B7A89','#070B12'),('#E9F0F5','#070B12')]
for a,b in pairs: print(a,'on',b, round(cr(a,b),2))
EOF
