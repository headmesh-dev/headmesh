import re
s = open('marks.py').read()
# A1: solid hub node with a halo instead of an eye-like ring
s = s.replace('''    defs = g + _a_mask(pid, 64)
    body = (
        f'<g fill="none" stroke="{p}" stroke-width="{w}" stroke-linecap="round">'
        f'<path d="M{AX0} {AY0}V{AY1}M{AX1} {AY0}V{AY1}"/>'
        f'<path mask="url(#{pid}-m)" d="M{AX0} {AY0}L{AX1} {AY1}M{AX1} {AY0}L{AX0} {AY1}"/>'
        f'<circle cx="{C}" cy="{C}" r="31" stroke-width="24"/></g>'
    )''', '''    defs = g + _a_mask(pid, 60)
    body = (
        f'<g fill="none" stroke="{p}" stroke-width="{w}" stroke-linecap="round">'
        f'<path d="M{AX0} {AY0}V{AY1}M{AX1} {AY0}V{AY1}"/>'
        f'<path mask="url(#{pid}-m)" d="M{AX0} {AY0}L{AX1} {AY1}M{AX1} {AY0}L{AX0} {AY1}"/></g>'
        f'<circle cx="{C}" cy="{C}" r="44" fill="{p}"/>'
    )''')
# B1 bigger satellites + thicker links
s = s.replace("big, mid, sml = (366, 92, 34), (452, 150, 23), (448, 50, 15)",
              "big, mid, sml = (364, 96, 40), (458, 156, 27), (452, 46, 18)")
s = s.replace("body = _h_solid(p, dx, dy) + _sats(p, [big, mid, sml], links, dx, dy)",
              "body = _h_solid(p, dx, dy) + _sats(p, [big, mid, sml], links, dx, dy, lw=13)")
# B2 ball-terminal h: full ascender ending in the head node
s = s.replace('''        f'd="M164 200V414M164 312A104 104 0 0 1 372 312V414"/>'
        f'<circle cx="164" cy="116" r="54" fill="{p}"/>\'''', '''        f'd="M164 130V414M164 312A104 104 0 0 1 372 312V414"/>'
        f'<circle cx="164" cy="112" r="56" fill="{p}"/>\'''')
s = s.replace("big, mid, sml = (338, 118, 30), (430, 92, 21), (420, 182, 14)\n    head = (164, 116)",
              "big, mid, sml = (330, 104, 34), (430, 78, 24), (424, 176, 16)\n    head = (164, 112)")
s = s.replace("body += _sats(p, [big, mid, sml], links, dx, dy, lw=8)", "body += _sats(p, [big, mid, sml], links, dx, dy, lw=12)")
# C1 compact weave
s = s.replace("d, b = _weave(pid, p, [(0, -72), (1, 72)], w=58, L=150, gap=13)", "d, b = _weave(pid, p, [(0, -70), (1, 70)], w=62, L=124, gap=13)")
# C2 thicker strands
s = s.replace("d, b = _weave(pid, p, [(0, -100), (1, 0), (2, 100)], w=40, L=300, gap=11, clip=f\"{pid}-clip\")",
              "d, b = _weave(pid, p, [(0, -104), (1, 0), (2, 104)], w=50, L=300, gap=12, clip=f\"{pid}-clip\")")
s = s.replace("d, b = _weave(pid, p, [(0, -74), (1, 74)], w=72, L=150, gap=12, outline=18)", "d, b = _weave(pid, p, [(0, -74), (1, 74)], w=76, L=128, gap=12, outline=20)")
open('marks.py','w').write(s)
