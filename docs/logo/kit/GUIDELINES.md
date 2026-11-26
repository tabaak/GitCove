# GitCove logo guidelines

**The idea:** a folded paper boat that keeps your work afloat until GitHub is back. The teal sail sits on a navy hull, with light falling from the right.

## Files

| File | Use |
|---|---|
| `gitcove-horizontal.svg` | Default logo: headers, README, docs |
| `gitcove-stacked.svg` | Square or tall spaces: posters, stickers, slides |
| `gitcove-symbol.svg` | The boat alone: avatars, when the name is already nearby |
| `gitcove-symbol-small.svg` | Two-colour boat for 16–48 px. The facets would blur at that size |
| `*-dark.svg` | For dark backgrounds (GitHub dark, terminals) |
| `gitcove-symbol-mono.svg`, `gitcove-horizontal-mono.svg` | One colour (navy), with the folds as gaps |
| `*-black.svg`, `*-white.svg` | One colour in black or white: stamps, engraving, single-colour print |
| `gitcove-wordmark.svg` | The name alone, when the boat already appears nearby |
| `web/` | `favicon.ico`, `favicon.svg` (switches to a light hull in dark tabs), app icons, `site.webmanifest`, and `head-snippet.html` to paste into `<head>` |
| `board/` | Presentation slides with mockups. `board-spec.json` rebuilds them |

## Colours

| | HEX | RGB | Role |
|---|---|---|---|
| Teal | `#0f8b8d` | 15 139 141 | Sail (shaded side), brand accent |
| Light teal | `#5fbcb6` | 95 188 182 | Sail (lit side) only |
| Navy | `#0b2530` | 11 37 48 | Hull, wordmark, app-icon tile |
| Navy 2 | `#1d4250` | 29 66 80 | Hull (lit side) only |

On dark backgrounds the sail is `#1fa8a6` / `#8fdcd6`, the hull `#c5d3d9` / `#ffffff`, and the wordmark `#e6edf3`.

- **Text contrast:** navy on white is 15.9:1. Teal on white is 4.1:1, so use it only for large text (it fails WCAG AA below 18 px). Light teal (2.2:1) is never used for text.
- **Print:** CMYK and Pantone matches have not been chosen yet. Have the printer match the HEX values above and approve a proof.

## Clear space and size

- **Clear space:** keep empty space on every side equal to the height of the hull at its centre fold. That's 46 units on the 256 grid, about a quarter of the boat's width.
- **Minimum size:** horizontal logo 100 px wide on screen (25 mm in print). Boat alone 16 px.
- **Below 48 px,** use `gitcove-symbol-small.svg`.

## Type

The wordmark is drawn as paths, so it needs no font or licence. It's a single-weight line with round letters, built on the same circles as the boat's proportions. Set other text in the interface's system font (`system-ui`), as GitCove's web interface already does.

## Don't

- Recolour the facets, or swap which side is lit
- Put the full-colour boat on teal, or on a mid-tone photo. Use the one-colour version or the dark version instead
- Stretch, rotate or outline the boat, or add shadows or gradients
- Retype "GitCove" in a font, or change the gap between the boat and the name
- Use the four-facet boat below 48 px

## Open items

- Trademark search: not done. Search trademark databases and run a reverse image search before wider use.
- The wordmark and the monoline letters are SVG strokes. For printing or cutting, convert them to outlines in a vector editor (Inkscape: Path → Stroke to Path).
- The old anchor files in `docs/logo/` (`gitcove-mark*`, `gitcove-badge*`) are still there.
