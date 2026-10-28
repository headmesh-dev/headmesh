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
