#!/usr/bin/env python3
"""Generate Gatehouse tray icons.

Windows tray icons must be real .ico files (energye/systray writes the bytes to a
temp file and hands them to LoadImage, which does not accept PNG). Each icon is
drawn supersampled and downscaled so the edges are antialiased at 16x16, which is
the size the notification area actually renders most of the time.

Run from the repo root:  python tools/genicons.py
"""

from PIL import Image, ImageDraw

OUT_DIR = "icons"
SS = 8  # supersample factor
SIZES = [16, 20, 24, 32, 48, 64, 128, 256]

# Shield silhouette, in 256x256 design space: flat shoulders, tapering to a point.
SHIELD = [
    (52, 30), (204, 30), (204, 118), (198, 158), (180, 190),
    (150, 214), (128, 228), (106, 214), (76, 190), (58, 158), (52, 118),
]

# state -> (fill, edge)
STATES = {
    "online":     ((34, 197, 94), (21, 128, 61)),    # green  - authenticated
    "connecting": ((245, 158, 11), (180, 83, 9)),    # amber  - logging in
    "error":      ((239, 68, 68), (153, 27, 27)),    # red    - login failed
    "nocampus":   ((100, 116, 139), (51, 65, 85)),   # slate  - internet down, not on campus
}


def scaled(points, f):
    return [(x * f, y * f) for x, y in points]


def draw_glyph(d, state, f, w):
    """White glyph centred in the shield. Kept deliberately blunt: at 16x16 a
    glyph has roughly 8 usable pixels, so anything intricate turns to mush."""
    white = (255, 255, 255, 255)
    cx, cy = 128 * f, 118 * f
    t = int(16 * f)  # stroke thickness

    if state == "online":
        # checkmark
        d.line(
            [(cx - 30 * f, cy - 2 * f), (cx - 9 * f, cy + 20 * f), (cx + 32 * f, cy - 26 * f)],
            fill=white, width=t, joint="curve",
        )
    elif state == "connecting":
        # three dots, sized to survive downsampling
        r = 7 * f
        for dx in (-30, 0, 30):
            d.ellipse(
                [cx + dx * f - r, cy - r, cx + dx * f + r, cy + r],
                fill=white,
            )
    elif state == "error":
        # exclamation mark
        d.line([(cx, cy - 34 * f), (cx, cy + 10 * f)], fill=white, width=t)
        d.ellipse(
            [cx - 9 * f, cy + 26 * f, cx + 9 * f, cy + 44 * f],
            fill=white,
        )
    elif state == "nocampus":
        # horizontal bar: "nothing to do"
        d.line([(cx - 34 * f, cy), (cx + 34 * f, cy)], fill=white, width=t)


def build(state, fill, edge):
    n = 256 * SS
    f = float(SS)
    img = Image.new("RGBA", (n, n), (0, 0, 0, 0))
    d = ImageDraw.Draw(img)

    d.polygon(scaled(SHIELD, f), fill=fill + (255,), outline=edge + (255,), width=int(9 * f))
    draw_glyph(d, state, f, int(16 * f))

    img = img.resize((256, 256), Image.LANCZOS)

    out = f"{OUT_DIR}/{state}.ico"
    img.save(out, format="ICO", sizes=[(s, s) for s in SIZES])
    print(f"  {out}  ({', '.join(str(s) for s in SIZES)})")


def main():
    import os
    os.makedirs(OUT_DIR, exist_ok=True)
    print("Generating Gatehouse tray icons:")
    for state, (fill, edge) in STATES.items():
        build(state, fill, edge)


if __name__ == "__main__":
    main()
