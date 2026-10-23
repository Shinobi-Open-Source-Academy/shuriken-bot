# Brand assets

| File | Size | Use |
|---|---|---|
| `shuriken-full.png` | 555×167 | Full lockup, **dark backgrounds only** |
| `shuriken-full-light.png` | 555×167 | Full lockup, light backgrounds (derived) |
| `shuriken-logo.png` | 168×168 | Mark alone, transparent, works on either background |
| `shuriken-avatar.png` | 512×512 | GitHub App profile picture (derived) |

## Palette

| Colour | Hex | Where |
|---|---|---|
| Navy | `#1E2432` | Mark outline, wordmark on light, avatar background |
| Off-white | `#F3F4F1` | Star fill, wordmark on dark |
| Orange | `#FF6A4D` | Centre dot |

## The two derived files

`shuriken-full.png` draws its wordmark in off-white, so on a white page the word
"shuriken" is invisible — only the mark survives. The two derived files exist to
fix that, and were generated from the originals, not redrawn:

- **`shuriken-full-light.png`** — the wordmark recoloured to navy. The original
  carries all of its anti-aliasing in the alpha channel over one flat colour, so
  recolouring the RGB and keeping the alpha is lossless. The mark is untouched.
- **`shuriken-avatar.png`** — the mark on an opaque navy square at 70% scale.
  Opaque because GitHub composites avatars against both light and dark chrome,
  and 70% so nothing clips where the avatar is cropped to a circle. Verified
  legible down to 20px, the size GitHub renders in a comment thread.

Regenerate both with:

```bash
python3 scripts/gen-brand-assets.py
```

If you later export a proper light-mode lockup from the design source, replace
`shuriken-full-light.png` and drop it from the script.

## Using the lockup in Markdown

GitHub honours `<picture>`, so the README swaps the lockup by theme:

```html
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/shuriken-full.png">
  <source media="(prefers-color-scheme: light)" srcset="assets/shuriken-full-light.png">
  <img src="assets/shuriken-full-light.png" alt="Shuriken" width="320">
</picture>
```

The `<img>` fallback must be the **light** file: renderers that ignore
`<picture>` tend to be on light backgrounds, and that is also the one case where
picking wrong makes the name disappear entirely.
