# runlight — brand

The mark: a **4×4 grid**, ten cells lit — they draw an upper-case **R**, the
rest stay off. Minimal on purpose: at 16 px it is still a clear letter.

## Source of truth

`internal/dashboard/web/` is canonical; the dashboard embeds and serves these,
so the app and the files can never disagree:

- `logo-light.svg` — the mark. Ink tile `#0b0b0b`, ten cells `#eb6834`,
  six `#232220`.
- `logo-dark.svg` — the same mark for dark surfaces, same idle grey.
  (Both files are identical today; the pair exists so a dark-only tweak — e.g.
  a lighter idle grey — is a one-value change in `logo-dark.svg` alone.)
- **one drawing at every size.** The tile, its 12.5% inset, the 12 px gaps
  and the cells (87 px) are all proportional to the 512 box, so a 16 px favicon and a
  512 px icon are the same shape scaled — never a different or tighter mark.
  The favicon is `logo-light.svg` itself (the dashboard links it directly);
  there is no separate favicon drawing to drift.
- `logo-lockup.svg` — mark + `runlight` wordmark (Space Grotesk Bold, outlined to paths so it needs no font) for light backgrounds: ink
  tile, `run` in ink `#0b0b0b`, `light` in orange.
- `logo-lockup-dark.svg` — the same lockup for dark backgrounds: ink tile,
  `run` in pure white `#ffffff`, `light` in orange.

The wordmark in both lockups is **Space Grotesk 700 as outlines** — the same
face the dashboard serves (`web/fonts/space-grotesk-latin-wght.woff2`), at the
UI's -0.03em tracking. No `font-family` in the SVG, so it renders identically
on any machine, in any design tool, and in the app.

`wordmark.json` holds those glyph paths; `outline.js` regenerates it from the
OFL font with `npm i fontkit`:

```sh
node outline.js   # writes wordmark.json: the run / light path groups + width
```

The dashboard picks the variant with `<picture>` + `prefers-color-scheme`;
the icon is the light mark in both schemes.

The PNGs in this folder are **derived** exports, regenerated from the SVGs
with headless Chrome (any rasteriser works) plus ffmpeg for the downscale.
Screenshot each SVG once at 1024 px, then scale down with lanczos — Chrome
crops the mark when you lower `--force-device-scale-factor` or shrink the
window, and writes a blank frame at exactly 128×128:

```sh
CHROME="/c/Program Files/Google/Chrome/Application/chrome.exe"
SRC="C:/Projects/runlight/internal/dashboard/web"
shot() { "$CHROME" --headless=new --disable-gpu --hide-scrollbars --no-first-run \
  --force-device-scale-factor=2 --default-background-color=00000000 \
  --window-size=$2 --screenshot="$1" "file:///$SRC/$3"; }
for v in light dark; do
  shot master-$v.png 512,512 logo-$v.svg
  for s in 512 256 128 64; do
    ffmpeg -v error -y -i master-$v.png -vf scale=$s:$s:flags=lanczos -pix_fmt rgba logo-$v-$s.png
  done
done
shot master-fav.png 512,512 logo-light.svg
for s in 64 32 16; do
  ffmpeg -v error -y -i master-fav.png -vf scale=$s:$s:flags=lanczos -pix_fmt rgba favicon-$s.png
done
rm -f master-*.png
```

## Colour

| token            | value     | use                          |
|------------------|-----------|------------------------------|
| ink              | `#0b0b0b` | tile, dark surfaces         |
| orange (`--rl`)  | `#eb6834` | the lit cells, `light`      |
| grey (idle)      | `#232220` | cells that are off          |

## Lockups

`lockup.png` (light) and `lockup-dark.png` (dark) are the 1400×400 exports of
the two lockup SVGs, made with the same master + lanczos recipe:

```sh
shot master-lockup.png 700,200 logo-lockup.svg   # or logo-lockup-dark.svg
ffmpeg -v error -y -i master-lockup.png -vf scale=1400:400:flags=lanczos -pix_fmt rgba lockup.png
```

The lockups carry the mark's ink tile, so they hold their shape on any
background of their tone; the wordmark sits outside the tile.

## Colour

| token            | value     | use                          |
|------------------|-----------|------------------------------|
| ink              | `#0b0b0b` | tile, dark surfaces         |
| orange (`--rl`)  | `#eb6834` | the lit cells, `light`      |
| grey (idle)      | `#232220` | cells that are off          |

## Lockups

`lockup.png` (light) and `lockup-dark.png` (dark) are the 1400×400 exports of
the two lockup SVGs, made with the same master + lanczos recipe:

```sh
shot master-lockup.png 700,200 logo-lockup.svg   # or logo-lockup-dark.svg
ffmpeg -v error -y -i master-lockup.png -vf scale=1400:400:flags=lanczos -pix_fmt rgba lockup.png
```

Both lockups are transparent outside the cells and the wordmark, so they sit
on any surface of their tone.

## Colour

| token            | value     | use                          |
|------------------|-----------|------------------------------|
| ink              | `#0b0b0b` | tile, dark surfaces         |
| orange (`--rl`)  | `#eb6834` | the lit cells, `light`      |
| grey (idle)      | `#232220` | cells that are off          |

## Lockups

`lockup.png` (light) and `lockup-dark.png` (dark) are the 1400×400 exports of
the two lockup SVGs, made with the same master + lanczos recipe:

```sh
shot master-lockup.png 700,200 logo-lockup.svg   # or logo-lockup-dark.svg
ffmpeg -v error -y -i master-lockup.png -vf scale=1400:400:flags=lanczos -pix_fmt rgba lockup.png
```

Use the dark one on `#0b0b0b`-or-darker surfaces, where the ink tile
disappears into the background and only the cells show.
