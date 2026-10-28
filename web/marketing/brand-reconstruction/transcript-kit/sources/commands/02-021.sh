python3 - <<'EOF'
import glob, os
rows=[]
for vdir in sorted(glob.glob('out/headmesh-brand/*/*/')):
    k=os.path.basename(vdir.rstrip('/'))
    s=lambda n: os.path.abspath(os.path.join(vdir,'svg',n))
    rows.append(f'''<div class="r"><div class="k">{k}</div>
    <div class="c dark"><img src="{s('lockup-horizontal-on-dark.svg')}" style="height:70px"></div>
    <div class="c light"><img src="{s('lockup-horizontal-on-light.svg')}" style="height:70px"></div>
    <img src="{s('app-icon-ink.svg')}" class="a"><img src="{s('app-icon-gradient.svg')}" class="a"><img src="{s('app-icon-paper.svg')}" class="a">
    <div class="c dark"><img src="{s('lockup-stacked-on-dark.svg')}" style="height:110px"></div>
    <img src="{os.path.abspath(os.path.join(vdir,'png','favicon-16.png'))}" style="width:16px;height:16px;image-rendering:pixelated;transform:scale(3);margin:0 24px">
    </div>''')
html='<html><body style="margin:0;background:#2a2f38;font:12px system-ui;color:#ccc"><style>.r{display:flex;align-items:center;gap:12px;padding:8px 12px}.k{width:150px}.c{padding:10px 14px;border-radius:8px}.dark{background:#070B12}.light{background:#F4F7F6}.a{width:72px;height:72px}</style>'+''.join(rows)+'</body></html>'
open('preview/kit.html','w').write(html)
EOF
node gen/render.js preview/kit.html preview/kit.png 1500 900 1 && python3 -c "from PIL import Image; im=Image.open('preview/kit.png'); print(im.size)"
