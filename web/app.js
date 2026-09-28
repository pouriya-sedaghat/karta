// Karta demo: loads the active release from the manifest, shows the
// release-pinned style and searches the same release. Every request goes to
// this page's origin; the page's CSP forbids any other destination.
import * as maplibregl from './vendor/maplibre-gl/maplibre-gl.mjs';

const status = document.getElementById('status');
const results = document.getElementById('results');
const form = document.getElementById('search');
const input = document.getElementById('q');

function setStatus(text) {
  status.textContent = text;
}

async function getJSON(url) {
  const res = await fetch(url, { headers: { Accept: 'application/json' } });
  const body = await res.json();
  if (!res.ok) {
    throw new Error(body?.error?.message || `${res.status} ${res.statusText}`);
  }
  return body;
}

let map;
let marker;
let releaseId;

async function start() {
  const manifest = await getJSON('/v1/manifest');
  releaseId = manifest.release.release_id;
  setStatus(`Release ${releaseId} · OSM data ${manifest.release.osm_data_timestamp}`);
  map = new maplibregl.Map({
    container: 'map',
    style: manifest.style_url,
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
    const body = await getJSON(`/v1/search?${params}`);
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
    setStatus(`Search failed: ${err.message}`);
  }
});

start().catch((err) => setStatus(`Could not load the map: ${err.message}`));
