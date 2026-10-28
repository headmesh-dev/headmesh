python3 - <<'EOF'
s=open('marks.py').read()
s=s.replace('''    """Fullmesh · Solid — heavy rounded strokes, hollow head ring."""
    p, g = _paint(paint, pid)
    w = 62
    defs = g + _a_mask(pid, 60)
    body = (
        f'<g fill="none" stroke="{p}" stroke-width="{w}" stroke-linecap="round">'
        f'<path d="M{AX0} {AY0}V{AY1}M{AX1} {AY0}V{AY1}"/>'
        f'<path mask="url(#{pid}-m)" d="M{AX0} {AY0}L{AX1} {AY1}M{AX1} {AY0}L{AX0} {AY1}"/></g>'
        f'<circle cx="{C}" cy="{C}" r="44" fill="{p}"/>'
    )''','''    """Fullmesh · Solid — heavy rounded strokes; the head is the hub where links cross."""
    p, g = _paint(paint, pid)
    w = 62
    defs = g
    body = (
        f'<g fill="none" stroke="{p}" stroke-width="{w}" stroke-linecap="round">'
        f'<path d="M{AX0} {AY0}V{AY1}M{AX1} {AY0}V{AY1}"/>'
        f'<path d="M{AX0} {AY0}L{AX1} {AY1}M{AX1} {AY0}L{AX0} {AY1}"/></g>'
        f'<circle cx="{C}" cy="{C}" r="52" fill="{p}"/>'
    )''')
open('marks.py','w').write(s)
EOF
cat > /home/claude/brand/gen/wm_test.py <<'EOF'
import sys, os
sys.path.insert(0, os.path.dirname(__file__))
from textpath import text_paths, fmt
from marks import MINT, CYAN, INK
rows = []
tests = [("Geist-600.ttf","headmesh",-0.035,4),("Geist-700.ttf","HeadMesh",-0.03,4),("Sora-700.ttf","HeadMesh",-0.02,4),
         ("Sora-600.ttf","headmesh",-0.03,4),("SpaceGrotesk-600.ttf","headmesh",-0.02,4),("SpaceGrotesk-700.ttf","HEADMESH",0.04,4),
         ("BricolageGrotesque-800.ttf","headmesh",-0.04,4),("BricolageGrotesque-700.ttf","HeadMesh",-0.03,4),("InstrumentSans-700.ttf","headmesh",-0.03,4),("GeistMono-500.ttf","headmesh",-0.02,4)]
y = 0
svg = []
for f, t, tr, sp in tests:
    tp = text_paths(t, f, 110, tracking=tr, x0=40, baseline=y + 120, split=sp)
    svg.append(f'<path fill="#fff" d="{fmt(tp["d_a"])}"/><path fill="url(#g)" d="{fmt(tp["d_b"])}"/><text x="700" y="{y+110}" fill="#789" font-size="22" font-family="monospace">{f} {tr}</text>')
    y += 150
open('/home/claude/brand/preview/wm.svg','w').write(f'<svg xmlns="http://www.w3.org/2000/svg" width="1100" height="{y+20}" viewBox="0 0 1100 {y+20}"><defs><linearGradient id="g" x1="0" x2="1"><stop offset="0" stop-color="{MINT}"/><stop offset="1" stop-color="{CYAN}"/></linearGradient></defs><rect width="100%" height="100%" fill="{INK}"/>{"".join(svg)}</svg>')
EOF
python3 /home/claude/brand/gen/wm_test.py && cd /home/claude/brand && node gen/render.js preview/wm.svg preview/wm.png 1100 1520 1
