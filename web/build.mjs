// Copies the demo and the pinned MapLibre GL JS distribution into dist/,
// which the API serves under /demo/. No bundler, no network access.
import { cpSync, mkdirSync, readFileSync, rmSync } from 'node:fs';

const pkg = JSON.parse(readFileSync('node_modules/maplibre-gl/package.json', 'utf8'));
const wanted = JSON.parse(readFileSync('package.json', 'utf8')).dependencies['maplibre-gl'];
if (pkg.version !== wanted) {
  throw new Error(`installed maplibre-gl ${pkg.version}, package.json pins ${wanted}; run npm ci`);
}
rmSync('dist', { recursive: true, force: true });
mkdirSync('dist/vendor/maplibre-gl', { recursive: true });
for (const f of ['index.html', 'app.js', 'app.css']) cpSync(f, `dist/${f}`);
for (const f of ['maplibre-gl.mjs', 'maplibre-gl-shared.mjs', 'maplibre-gl-worker.mjs', 'maplibre-gl.css']) {
  cpSync(`node_modules/maplibre-gl/dist/${f}`, `dist/vendor/maplibre-gl/${f}`);
}
cpSync('node_modules/maplibre-gl/LICENSE.txt', 'dist/vendor/maplibre-gl/LICENSE.txt');
console.log(`demo built with maplibre-gl ${pkg.version}`);
