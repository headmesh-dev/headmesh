"""All 13 mark variants. A1/D4 retain recovered code; other bases are documented reconstructions."""

import math

C = 256
AX0, AX1, AY0, AY1 = 140, 372, 112, 400
MINT, CYAN, INK = "#3CEFB4", "#27BFF5", "#070B12"
PAPER, EVERGREEN, BLUE = "#F4F7F6", "#0A7A5C", "#0A70B0"


def _paint(paint, pid):
    if paint not in ("grad", "light-grad"):
        return paint, ""
    first, second = (EVERGREEN, BLUE) if paint == "light-grad" else (MINT, CYAN)
    gradient = f'<linearGradient id="{pid}-g" gradientUnits="userSpaceOnUse" x1="72" y1="72" x2="440" y2="440"><stop offset="0" stop-color="{first}"/><stop offset="1" stop-color="{second}"/></linearGradient>'
    return f"url(#{pid}-g)", gradient


def mark_A1(paint="grad", pid="a1"):
    """Fullmesh · Solid — heavy rounded strokes; the head is the hub where links cross."""
    p, g = _paint(paint, pid)
    w = 62
    defs = g
    body = (
        f'<g fill="none" stroke="{p}" stroke-width="{w}" stroke-linecap="round">'
        f'<path d="M{AX0} {AY0}V{AY1}M{AX1} {AY0}V{AY1}"/>'
        f'<path d="M{AX0} {AY0}L{AX1} {AY1}M{AX1} {AY0}L{AX0} {AY1}"/></g>'
        f'<circle cx="{C}" cy="{C}" r="52" fill="{p}"/>'
    )
    return defs, body


def mark_A2(paint="grad", pid="a2"):
    p, defs = _paint(paint, pid)
    defs += f'<mask id="{pid}-hub" maskUnits="userSpaceOnUse" x="0" y="0" width="512" height="512"><rect width="512" height="512" fill="white"/><circle cx="256" cy="256" r="58" fill="black"/></mask>'
    body = f'<g stroke="{p}" fill="none" stroke-width="18" stroke-linecap="round"><path d="M128 116V396M384 116V396"/><path mask="url(#{pid}-hub)" d="M128 116L384 396M384 116L128 396"/><circle cx="256" cy="256" r="34"/></g>'
    body += ''.join(f'<circle cx="{x}" cy="{y}" r="46" fill="{p}"/>' for x, y in [(128, 116), (384, 116), (128, 396), (384, 396)])
    return defs, body


def mark_A3(paint="grad", pid="a3"):
    p, defs = _paint(paint, pid)
    defs += f'<mask id="{pid}-hub" maskUnits="userSpaceOnUse" x="0" y="0" width="512" height="512"><rect width="512" height="512" fill="white"/><circle cx="256" cy="256" r="60" fill="black"/></mask>'
    body = f'<g fill="{p}"><rect x="100" y="96" width="104" height="320" rx="52"/><rect x="308" y="96" width="104" height="320" rx="52"/></g><path mask="url(#{pid}-hub)" d="M152 148L360 364M360 148L152 364" stroke="{p}" stroke-width="62" stroke-linecap="round"/><circle cx="256" cy="256" r="33" fill="none" stroke="{p}" stroke-width="27"/>'
    return defs, body


def _h_solid(p):
    return f'<path d="M156 416V156M156 312A104 104 0 0 1 364 312V416" fill="none" stroke="{p}" stroke-width="72" stroke-linecap="round"/>'


def _sats(p, circles, links, lw):
    body = ''.join(f'<path d="M{a[0]} {a[1]}L{b[0]} {b[1]}" stroke="{p}" stroke-width="{lw}" stroke-linecap="round"/>' for a, b in links)
    return body + ''.join(f'<circle cx="{x}" cy="{y}" r="{r}" fill="{p}"/>' for x, y, r in circles)


def mark_B1(paint="grad", pid="b1"):
    p, defs = _paint(paint, pid)
    # The absent _sats helper's placement cannot be replayed. Align the small
    # satellite to the recorded final bbox; the replacement string alone has y=46.
    big, mid, small = (364, 96, 40), (458, 156, 27), (452, 74, 18)
    body = _h_solid(p) + _sats(p, [big, mid, small], [((156, 156), big), (big, mid), (big, small), (small, mid)], 13)
    return defs, f'<g transform="translate(-30 -2)">{body}</g>'


def mark_B2(paint="grad", pid="b2"):
    p, defs = _paint(paint, pid)
    body = f'<path d="M164 130V414M164 312A104 104 0 0 1 372 312V414" fill="none" stroke="{p}" stroke-width="56" stroke-linecap="round"/><circle cx="164" cy="112" r="56" fill="{p}"/>'
    # Two-pixel placement correction in the reconstructed helper matches the
    # final recorded bbox; no original helper body is present in the transcript.
    big, mid, small, head = (330, 104, 34), (430, 80, 24), (424, 176, 16), (164, 112)
    body += _sats(p, [big, mid, small], [(head, big), (big, mid), (mid, small), (big, small)], 12)
    return defs, f'<g transform="translate(-26 16)">{body}</g>'


def mark_B3(paint="grad", pid="b3"):
    p, defs = _paint(paint, pid)
    defs += f'<mask id="{pid}-cut" maskUnits="userSpaceOnUse" x="0" y="0" width="512" height="512"><rect width="512" height="512" fill="white"/><g transform="translate(76 108) scale(.64)">{_h_solid("black")}</g></mask>'
    body = f'<circle cx="246" cy="270" r="184" fill="{p}" mask="url(#{pid}-cut)"/>'
    nodes = [(354, 72, 17), (408, 117, 22), (448, 184, 17)]
    body += _sats(p, nodes, [(nodes[0], nodes[1]), (nodes[1], nodes[2]), ((400, 224), nodes[2])], 9)
    return defs, body


def _weave(pid, paint, strands, w, L, gap, outline=0, clip=None):
    defs, paths = "", []
    if paint.startswith("url(#"):
        parent_gradient = paint[5:-1]
        local_gradient = f"{pid}-weave-g"
        defs += f'<linearGradient id="{local_gradient}" href="#{parent_gradient}" gradientTransform="rotate(-45) translate(-256 -256)"/>'
        paint = f"url(#{local_gradient})"
    positions = [offset for _, offset in strands]
    for orientation in (0, 1):
        for index, offset in enumerate(positions):
            name = f"{pid}-strand-{orientation}-{index}"
            path = f"M{offset} {-L}V{L}" if orientation == 0 else f"M{-L} {offset}H{L}"
            mask = f'<mask id="{name}" maskUnits="userSpaceOnUse" x="-600" y="-600" width="1200" height="1200"><rect x="-600" y="-600" width="1200" height="1200" fill="white"/>'
            if outline:
                mask += f'<path d="{path}" fill="none" stroke="black" stroke-width="{w - outline * 2}" stroke-linecap="round"/>'
            for crossing, other in enumerate(positions):
                if (index + crossing) % 2 == orientation:
                    x, y = (offset, other) if orientation == 0 else (other, offset)
                    mask += f'<rect x="{x - w / 2 - gap}" y="{y - w / 2 - gap}" width="{w + gap * 2}" height="{w + gap * 2}" fill="black"/>'
            defs += mask + "</mask>"
            paths.append(f'<path d="{path}" stroke="{paint}" stroke-width="{w}" stroke-linecap="round" mask="url(#{name})"/>')
    body = f'<g transform="translate(256 256) rotate(45)" fill="none">{"".join(paths)}</g>'
    if clip:
        body = f'<g clip-path="url(#{clip})">{body}</g>'
    return defs, body


def mark_C1(paint="grad", pid="c1"):
    p, gradient = _paint(paint, pid)
    defs, body = _weave(pid, p, [(0, -70), (1, 70)], w=62, L=124, gap=13)
    return gradient + defs, body


def mark_C2(paint="grad", pid="c2"):
    p, gradient = _paint(paint, pid)
    clip = f'<clipPath id="{pid}-clip"><rect x="76" y="76" width="360" height="360" rx="94"/></clipPath>'
    defs, body = _weave(pid, p, [(0, -104), (1, 0), (2, 104)], w=50, L=300, gap=12, clip=f"{pid}-clip")
    body += f'<rect x="88" y="88" width="336" height="336" rx="82" fill="none" stroke="{p}" stroke-width="24"/>'
    return gradient + clip + defs, body


def mark_C3(paint="grad", pid="c3"):
    p, gradient = _paint(paint, pid)
    defs, body = _weave(pid, p, [(0, -74), (1, 74)], w=76, L=128, gap=12, outline=20)
    return gradient + defs, body


def mark_D4(paint="grad", pid="d4", fg=None):
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


def mark_D1(paint="grad", pid="d1"):
    return mark_D4(paint, pid)


def mark_D2(paint="grad", pid="d2"):
    p, defs = _paint(paint, pid)
    body = f'<g stroke="{p}" stroke-width="68" stroke-linecap="round" fill="none"><path d="M122 238V400M390 238V400M122 312A67 67 0 0 1 256 312A67 67 0 0 1 390 312M256 312V400"/></g><circle cx="122" cy="120" r="44" fill="{p}"/><circle cx="390" cy="120" r="44" fill="{p}"/>'
    return defs, body


def mark_D3(paint="grad", pid="d3"):
    p, defs = _paint(paint, pid)
    y = 280 - math.sqrt(101 ** 2 - 33 ** 2)
    middle = 280 - math.sqrt(101 ** 2 - 67 ** 2)
    outline = f'M88 390V122A34 34 0 0 1 156 122V{y}A101 101 0 0 1 256 {middle}A101 101 0 0 1 356 {y}V122A34 34 0 0 1 424 122V390A34 34 0 0 1 356 390V280A33 33 0 0 0 290 280V390A34 34 0 0 1 222 390V280A33 33 0 0 0 156 280V390A34 34 0 0 1 88 390Z'
    factor = 335.6 / 358
    body = f'<path transform="translate(256 256) scale({factor}) translate(-256 -256)" d="{outline}" fill="none" stroke="{p}" stroke-width="22" stroke-linejoin="round"/>'
    return defs, body


MARKS = {
    "A1": ("Fullmesh", "Solid", mark_A1),
    "A2": ("Fullmesh", "Schematic", mark_A2),
    "A3": ("Fullmesh", "Pillars", mark_A3),
    "B1": ("Head node", "Glyph", mark_B1),
    "B2": ("Head node", "Node h", mark_B2),
    "B3": ("Head node", "Badge", mark_B3),
    "C1": ("Weave", "Weave", mark_C1),
    "C2": ("Weave", "Net", mark_C2),
    "C3": ("Weave", "Ribbon", mark_C3),
    "D1": ("Arch", "Monogram", mark_D1),
    "D2": ("Arch", "Two heads", mark_D2),
    "D3": ("Arch", "Tunnel", mark_D3),
    "D4": ("Arch", "Shared leg", mark_D4),
}

RECORDED_BBOXES = {
    "A1": [109, 81, 403, 431], "A2": [82, 70, 430, 442], "A3": [100, 96, 412, 416],
    "B1": [90, 54, 455, 450], "B2": [82, 72, 428, 458], "B3": [62, 55, 465, 454],
    "C1": [87, 87, 425, 425], "C2": [76, 76, 436, 436], "C3": [75, 75, 437, 437],
    "D1": [88, 88, 424, 424], "D2": [78, 76, 434, 434], "D3": [88, 88, 424, 424],
    "D4": [88, 88, 424, 424],
}


def icon_svg(key, paint="grad", pid=None, fg=None):
    function = MARKS[key][2]
    kwargs = {"paint": paint, "pid": pid or key.lower()}
    if key == "D4":
        kwargs["fg"] = fg
    defs, body = function(**kwargs)
    return f'<svg xmlns="http://www.w3.org/2000/svg" width="512" height="512" viewBox="0 0 512 512"><defs>{defs}</defs>{body}</svg>'
