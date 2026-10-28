"""Reconstructed text_paths/formatting API: HarfBuzz shaping and fontTools SVG outlines."""

import re
from functools import lru_cache
from io import BytesIO
from pathlib import Path

import uharfbuzz as hb
from fontTools.pens.boundsPen import BoundsPen
from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.transformPen import TransformPen
from fontTools.ttLib import TTFont

FONT_DIR = Path(__file__).resolve().parents[1] / "fonts"


@lru_cache(maxsize=32)
def _font(filename):
    font = TTFont(FONT_DIR / filename)
    font.flavor = None
    stream = BytesIO()
    font.save(stream)
    return font, stream.getvalue()


def fmt(data, precision=2):
    def number(match):
        value = round(float(match.group()), precision)
        return f"{value:.{precision}f}".rstrip("0").rstrip(".") if precision else str(int(value))
    return re.sub(r"-?\d*\.\d+(?:[eE][-+]?\d+)?", number, data)


def text_paths(text, filename, size, tracking=0, x0=0, baseline=0, split=None):
    font, binary = _font(filename)
    units = font["head"].unitsPerEm
    scale = size / units
    glyphs = font.getGlyphSet()
    hb_font = hb.Font(hb.Face(binary))
    hb.ot_font_set_funcs(hb_font)
    hb_font.scale = (units, units)
    buffer = hb.Buffer()
    buffer.add_str(text)
    buffer.guess_segment_properties()
    hb.shape(hb_font, buffer, {"kern": True})
    paths, first, second = [], [], []
    minimum_x = minimum_y = float("inf")
    maximum_x = maximum_y = float("-inf")
    cursor = 0
    for info, position in zip(buffer.glyph_infos, buffer.glyph_positions):
        name = font.getGlyphName(info.codepoint)
        transform = (scale, 0, 0, -scale, x0 + (cursor + position.x_offset) * scale,
                     baseline - position.y_offset * scale)
        pen = SVGPathPen(glyphs)
        glyphs[name].draw(TransformPen(pen, transform))
        data = pen.getCommands()
        paths.append(data)
        (first if split is None or info.cluster < split else second).append(data)
        bounds = BoundsPen(glyphs)
        glyphs[name].draw(TransformPen(bounds, transform))
        if bounds.bounds:
            left, top, right, bottom = bounds.bounds
            minimum_x, minimum_y = min(minimum_x, left), min(minimum_y, top)
            maximum_x, maximum_y = max(maximum_x, right), max(maximum_y, bottom)
        cursor += position.x_advance + tracking * units
    return {"d": " ".join(paths), "d_a": " ".join(first), "d_b": " ".join(second),
            "bounds": (minimum_x, minimum_y, maximum_x, maximum_y),
            "advance": cursor * scale - tracking * size}
