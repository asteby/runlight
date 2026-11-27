// Bundles src/main.jsx into ../web/app.js (single minified ESM file, embedded by the Go binary).
//   npm run build   production bundle (no sourcemap)
//   npm run dev     watch mode, with sourcemap (../web/app.js.map)
//
// Imports with the `optional:` prefix (e.g. 'optional:./pages/Alerts.jsx') resolve to the real file when it
// exists, or to an empty stub module (default = null, meta = null, register = null) when it does not, so modules that other
// people own never break the build.
import * as esbuild from 'esbuild';
import fs from 'node:fs';
import path from 'node:path';
import zlib from 'node:zlib';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const outfile = path.join(here, '..', 'web', 'app.js');
const watch = process.argv.includes('--watch');

const optional = {
  name: 'optional',
  setup(b) {
    b.onResolve({ filter: /^optional:/ }, (args) => {
      const file = path.resolve(args.resolveDir, args.path.slice('optional:'.length));
      return fs.existsSync(file) ? { path: file } : { path: file, namespace: 'optional-missing' };
    });
    b.onLoad({ filter: /.*/, namespace: 'optional-missing' }, () => ({
      contents: 'export default null; export const meta = null; export const register = null;',
      loader: 'js',
    }));
  },
};

const options = {
  entryPoints: [path.join(here, 'src', 'main.jsx')],
  outfile,
  bundle: true,
  format: 'esm',
  target: 'es2022',
  minify: !watch,
  sourcemap: watch,
  jsx: 'automatic',
  loader: { '.js': 'jsx' },
  legalComments: 'none',
  define: { 'process.env.NODE_ENV': JSON.stringify(watch ? 'development' : 'production') },
  plugins: [optional],
  logLevel: 'info',
};

if (watch) {
  const ctx = await esbuild.context(options);
  await ctx.watch();
  console.log('watching src/ -> web/app.js');
} else {
  if (fs.existsSync(outfile + '.map')) fs.rmSync(outfile + '.map');
  await esbuild.build(options);
  const buf = fs.readFileSync(outfile);
  const kb = (n) => (n / 1024).toFixed(1) + ' KB';
  console.log(`app.js: ${kb(buf.length)} (${kb(zlib.gzipSync(buf, { level: 9 }).length)} gzip)`);
  if (buf.length > 200 * 1024) console.warn("aviso: app.js supera el presupuesto orientativo de 200 KB (react-dom 19 pesa ~205 KB por si solo)");
  if (buf.length > 400 * 1024) { console.error("app.js supera el limite duro de 400 KB"); process.exit(1); }
}
