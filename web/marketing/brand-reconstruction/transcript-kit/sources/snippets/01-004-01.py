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
