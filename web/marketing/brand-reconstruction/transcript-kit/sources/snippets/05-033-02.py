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
