#!/usr/bin/env python3
"""Regenerate the derived brand assets from the two originals.

Inputs  (hand-made, never written by this script):
    assets/shuriken-full.png    full lockup, wordmark in off-white
    assets/shuriken-logo.png    mark alone, transparent

Outputs (overwritten):
    assets/shuriken-full-light.png   lockup for light backgrounds
    assets/shuriken-avatar.png       GitHub App profile picture

Requires Pillow:  python3 -m pip install pillow
"""

import pathlib
import sys

try:
    from PIL import Image
except ImportError:
    sys.exit("Pillow is not installed: python3 -m pip install pillow")

ROOT = pathlib.Path(__file__).resolve().parent.parent
ASSETS = ROOT / "assets"

NAVY = (30, 36, 50)

# The lockup has a wide empty gutter between the mark and the wordmark; the
# split is taken inside it so the mark is never touched. Asserted below rather
# than assumed, because a redrawn lockup could move it.
SPLIT = 182
AVATAR_SIZE = 512
AVATAR_SCALE = 0.70  # padding keeps the mark clear of a circular crop


def wordmark_gutter(im):
    """Return the (start, end) of the widest fully transparent column run."""
    px = im.load()
    w, h = im.size
    empty = [all(px[x, y][3] <= 10 for y in range(h)) for x in range(w)]
    best = run = None
    for x, blank in enumerate(empty):
        if blank:
            run = (run[0], x) if run else (x, x)
            if best is None or run[1] - run[0] > best[1] - best[0]:
                best = run
        else:
            run = None
    return best


def build_light_lockup():
    im = Image.open(ASSETS / "shuriken-full.png").convert("RGBA")
    gutter = wordmark_gutter(im)
    if not (gutter and gutter[0] < SPLIT < gutter[1]):
        sys.exit(
            f"SPLIT={SPLIT} is not inside the lockup's gutter {gutter}. "
            "The source lockup changed; update SPLIT before regenerating."
        )

    px = im.load()
    w, h = im.size
    for y in range(h):
        for x in range(SPLIT, w):
            alpha = px[x, y][3]
            if alpha:
                # The wordmark is one flat colour anti-aliased purely in alpha,
                # so replacing RGB and keeping alpha recolours it losslessly.
                px[x, y] = (*NAVY, alpha)

    out = ASSETS / "shuriken-full-light.png"
    im.save(out)
    print(f"wrote {out.relative_to(ROOT)} ({w}x{h})")


def build_avatar():
    mark = Image.open(ASSETS / "shuriken-logo.png").convert("RGBA")
    mark = mark.crop(mark.getbbox())

    target = int(AVATAR_SIZE * AVATAR_SCALE)
    scale = target / max(mark.size)
    mark = mark.resize(
        (round(mark.size[0] * scale), round(mark.size[1] * scale)), Image.LANCZOS
    )

    av = Image.new("RGBA", (AVATAR_SIZE, AVATAR_SIZE), (*NAVY, 255))
    av.alpha_composite(
        mark,
        ((AVATAR_SIZE - mark.size[0]) // 2, (AVATAR_SIZE - mark.size[1]) // 2),
    )

    out = ASSETS / "shuriken-avatar.png"
    av.convert("RGB").save(out)
    print(f"wrote {out.relative_to(ROOT)} ({AVATAR_SIZE}x{AVATAR_SIZE})")


if __name__ == "__main__":
    build_light_lockup()
    build_avatar()
