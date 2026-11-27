const fk = require('fontkit');
const f = fk.openSync('C:/tmp/fk/sg.ttf');
const v = f.getVariation({ wght: 700 });
const SIZE = 108, TRACK = -3.24, X = 204, BASE = 140;
const s = SIZE / v.unitsPerEm;
const run = v.layout('runlight');
let pen = 0;
const out = [];
for (let i = 0; i < run.glyphs.length; i++) {
  const g = run.glyphs[i];
  const d = g.path.toSVG();
  if (d) out.push({ d: d, x: pen * s });
  pen += run.positions[i].xAdvance + TRACK / s;
}
const parts = out.map(o =>
  '<path transform="translate(' + (X + o.x).toFixed(2) + ' ' + BASE + ') scale(' +
  s.toFixed(6) + ' ' + (-s).toFixed(6) + ')" d="' + o.d + '"/>');
// "run" is the first three glyphs; the rest spells "light".
const cut = parts.slice(0, 3).join('');
const rest = parts.slice(3).join('');
require('fs').writeFileSync('C:/tmp/fk/outline.json',
  JSON.stringify({ width: pen * s, run: cut, light: rest }));
console.log('width', (pen * s).toFixed(1), 'glyph paths', parts.length);
