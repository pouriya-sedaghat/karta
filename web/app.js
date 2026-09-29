// Karta demo: loads the active release from the manifest, shows its
// content-addressed style and searches the same release. Every request goes to
// this page's origin; the page's CSP forbids any other destination.
import * as maplibregl from './vendor/maplibre-gl/maplibre-gl.mjs';

// API paths are resolved against this module (<base>/demo/app.js), never the
// host root, so the demo also works when a reverse proxy serves Karta under a
// path prefix: https://example.com/maps/demo/ calls
// https://example.com/maps/v1/manifest. Style, tile and glyph URLs come from
// the API and are absolute under KARTA_PUBLIC_BASE_URL.
const apiURL = (path) => new URL(`../${path}`, import.meta.url);

const status = document.getElementById('status');
const results = document.getElementById('results');
const form = document.getElementById('search');
const input = document.getElementById('q');

function setStatus(text) {
  status.textContent = text;
}

async function getJSON(url, cache = 'default') {
  const res = await fetch(url, { headers: { Accept: 'application/json' }, cache });
  const body = await res.json();
  if (!res.ok) {
    const err = new Error(body?.error?.message || `${res.status} ${res.statusText}`);
    err.code = body?.error?.code;
    throw err;
  }
  return body;
}

// Style URLs are content-addressed and change when the API is upgraded or
// reconfigured, so a manifest fetched just before such a change can name a
// style URL the server no longer serves (404 unknown_style, 404 unknown_release,
// or 410 release_expired once a replaced release's pin grace period ended).
// Then the page refetches the manifest, bypassing the HTTP cache, and loads the
// style it names: at most STYLE_ATTEMPTS times, so a persistent error ends with
// a message instead of a loop.
const STYLE_ATTEMPTS = 3;
const RETRY_DELAY_MS = 250;
const STALE_STYLE_CODES = new Set(['unknown_style', 'unknown_release', 'release_expired']);

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

async function loadManifestAndStyle() {
  let lastError;
  for (let attempt = 1; attempt <= STYLE_ATTEMPTS; attempt++) {
    const manifest = await getJSON(apiURL('v1/manifest'), attempt === 1 ? 'no-cache' : 'reload');
    const res = await fetch(manifest.style_url, { headers: { Accept: 'application/json' } });
    const body = await res.json().catch(() => null);
    if (res.ok && body) {
      return { manifest, style: body };
    }
    lastError = new Error(body?.error?.message || `style: ${res.status} ${res.statusText}`);
    if ((res.status !== 404 && res.status !== 410) || !STALE_STYLE_CODES.has(body?.error?.code)) {
      break;
    }
    if (attempt < STYLE_ATTEMPTS) {
      await sleep(RETRY_DELAY_MS * attempt);
    }
  }
  throw lastError;
}

let map;
let marker;
let releaseId;

async function start() {
  const { manifest, style } = await loadManifestAndStyle();
  // Search uses the release of the manifest whose style is shown.
  releaseId = manifest.release.release_id;
  setStatus(`Release ${releaseId} · OSM data ${manifest.release.osm_data_timestamp}`);
  map = new maplibregl.Map({
    container: 'map',
    style,
    center: manifest.default_view.center,
    zoom: manifest.default_view.zoom,
    hash: true,
    attributionControl: { compact: false },
  });
  map.addControl(new maplibregl.NavigationControl({ showCompass: false }), 'top-right');
  map.addControl(new maplibregl.ScaleControl(), 'bottom-right');
  window.kartaMap = map;
  window.kartaRelease = releaseId;
  map.on('error', (e) => {
    window.kartaErrors = (window.kartaErrors || []).concat(String(e.error?.message || e.error));
  });
}

function show(result) {
  const [w, s, e, n] = result.bbox;
  if (e - w > 1e-6 || n - s > 1e-6) {
    map.fitBounds([[w, s], [e, n]], { padding: 60, maxZoom: 17 });
  } else {
    map.flyTo({ center: [result.lon, result.lat], zoom: 17 });
  }
  if (marker) marker.remove();
  marker = new maplibregl.Marker().setLngLat([result.lon, result.lat]).addTo(map);
}

form.addEventListener('submit', async (event) => {
  event.preventDefault();
  const q = input.value.trim();
  if (!q || !releaseId) return;
  results.replaceChildren();
  setStatus('Searching…');
  try {
    const params = new URLSearchParams({ q, limit: '10', release_id: releaseId });
    const body = await getJSON(apiURL(`v1/search?${params}`));
    setStatus(`${body.results.length} result(s) for “${body.query}”`);
    for (const r of body.results) {
      const li = document.createElement('li');
      const button = document.createElement('button');
      button.type = 'button';
      const name = document.createElement('bdi');
      name.textContent = r.display_name;
      name.dir = 'auto';
      const meta = document.createElement('span');
      meta.className = 'meta';
      meta.textContent = `${r.category}=${r.subcategory} · ${r.id} · matched ${r.match.key} (${r.match.type})`;
      button.append(name, meta);
      button.addEventListener('click', () => show(r));
      li.append(button);
      results.append(li);
    }
  } catch (err) {
    if (err.code === 'release_expired') {
      // The release this page shows was replaced and its pin grace period
      // ended: reload onto the current release (the URL hash keeps the view).
      setStatus('The map data was updated; reloading…');
      setTimeout(() => window.location.reload(), 1000);
      return;
    }
    setStatus(`Search failed: ${err.message}`);
  }
});

start().catch((err) => setStatus(`Could not load the map: ${err.message}`));
