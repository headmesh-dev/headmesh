s=open('build.py').read()
start=s.index('def constellation(')
end=s.index('def social_card(')
new='''def constellation(x, y, w, h, seed=7, n=34, key="D1", label=True):
    """Brand motif — two tenants, one grant. Each workspace is a sealed region of meshed nodes; exactly one link
    crosses between them, and it glows. Everything else stays private."""
    rnd = random.Random(seed)
    gap = w * 0.16
    wa = (w - gap) * 0.58
    wb = (w - gap) - wa
    regions = [(x, y, wa, h, 0), (x + wa + gap, y + h * 0.16, wb, h * 0.68, 1)]
    defs, body = "", ""
    pts = []
    for (rx, ry, rw, rh, side) in regions:
        body += (f'<rect x="{rx:.1f}" y="{ry:.1f}" width="{rw:.1f}" height="{rh:.1f}" rx="{min(rw, rh) * 0.12:.1f}" '
                 f'fill="#0B111A" stroke="#1C2736" stroke-width="1.5"/>')
        cols, rows_ = (4, 3) if side == 0 else (2, 3)
        mx, my = rw * 0.14, rh * 0.17
        for i in range(cols):
            for j in range(rows_):
                if rnd.random() < 0.18:
                    continue
                px = rx + mx + (rw - 2 * mx) * (i + 0.5 + (rnd.random() - 0.5) * 0.55) / cols
                py = ry + my + (rh - 2 * my) * (j + 0.5 + (rnd.random() - 0.5) * 0.55) / rows_
                pts.append((px, py, side))
    lines = ""
    for i, (ax, ay, sa) in enumerate(pts):
        ds = sorted((math.hypot(ax - bx, ay - by), j) for j, (bx, by, sb) in enumerate(pts) if j != i and sb == sa)
        for dist, j in ds[:2]:
            if j > i:
                bx, by, _ = pts[j]
                lines += f'<line x1="{ax:.1f}" y1="{ay:.1f}" x2="{bx:.1f}" y2="{by:.1f}"/>'
    a_side = [p for p in pts if p[2] == 0]
    b_side = [p for p in pts if p[2] == 1]
    cy = y + h / 2
    left = min(a_side, key=lambda p: -p[0] + abs(p[1] - cy) * 0.6)
    right = min(b_side, key=lambda p: p[0] + abs(p[1] - cy) * 0.6)
    gid = uid("cg")
    defs += (f'<linearGradient id="{gid}" gradientUnits="userSpaceOnUse" x1="{left[0]:.1f}" y1="0" x2="{right[0]:.1f}" y2="0">'
             f'<stop offset="0" stop-color="{MINT}"/><stop offset="1" stop-color="{CYAN}"/></linearGradient>')
    body += f'<g stroke="#223044" stroke-width="1.5">{lines}</g>'
    body += "".join(f'<circle cx="{px:.1f}" cy="{py:.1f}" r="{max(3.0, w * 0.0075):.1f}" fill="#334357"/>'
                    for px, py, s in pts if (px, py, s) not in (left, right))
    body += (f'<path d="M{left[0]:.1f} {left[1]:.1f}L{right[0]:.1f} {right[1]:.1f}" stroke="url(#{gid})" '
             f'stroke-width="{max(2.5, w * 0.006):.1f}" stroke-dasharray="9 7" stroke-linecap="round" fill="none"/>')
    rr = max(6.0, w * 0.013)
    for p, c in ((left, MINT), (right, CYAN)):
        body += (f'<circle cx="{p[0]:.1f}" cy="{p[1]:.1f}" r="{rr * 2.4:.1f}" fill="{c}" opacity=".14"/>'
                 f'<circle cx="{p[0]:.1f}" cy="{p[1]:.1f}" r="{rr:.1f}" fill="{c}"/>')
    if label:
        fs = max(11.0, w * 0.026)
        t = text_paths("grant · 4h", MONO_FONT, fs)
        tw = t["bounds"][2] - t["bounds"][0]
        mx_, my_ = (left[0] + right[0]) / 2, (left[1] + right[1]) / 2 - fs * 1.5
        px, py = mx_ - tw / 2 - fs * 0.6, my_ - fs * 0.95
        body += (f'<rect x="{px:.1f}" y="{py:.1f}" width="{tw + fs * 1.2:.1f}" height="{fs * 1.6:.1f}" rx="{fs * 0.8:.1f}" '
                 f'fill="#0E2A24" stroke="{MINT}" stroke-opacity=".45"/>')
        t = text_paths("grant · 4h", MONO_FONT, fs, x0=mx_ - tw / 2, baseline=py + fs * 1.13)
        body += f'<path fill="{MINT}" d="{fmt(t["d"])}"/>'
    return defs, body


'''
s=s[:start]+new+s[end:]
# OG layout tweaks: motif placement and spacing
s=s.replace('cd, cb = constellation(W * 0.52, H * 0.08, W * 0.5, H * 0.62, seed=11)','cd, cb = constellation(W * 0.56, H * 0.13, W * 0.37, H * 0.36, seed=11)')
s=s.replace('''        y = H * 0.56
        for i, line in enumerate(["Share machines,", "not your network."]):''','''        y = H * 0.545
        for i, line in enumerate(["Share machines,", "not your network."]):''')
s=s.replace('''x0=m, baseline=y + hs * 1.06 + H * 0.105)''','''x0=m, baseline=y + hs * 1.06 + H * 0.098)''')
s=s.replace('''        fy = H - m * 0.95
        body += f'<rect x="{m}" y="{fy - H * 0.075:.1f}" width="{W - 2 * m}" height="2" fill="{LINE}"/>\'''','''        fy = H - m * 0.92
        body += f'<rect x="{m}" y="{fy - H * 0.068:.1f}" width="{W - 2 * m}" height="2" fill="{LINE}"/>\'''')
s=s.replace('cd, cb = constellation(W * 0.40, -H * 0.05, W * 0.62, H * 1.1, seed=5, n=44)','cd, cb = constellation(W * 0.52, H * 0.16, W * 0.40, H * 0.68, seed=5)')
open('build.py','w').write(s)
