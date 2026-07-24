// tachyne-map browser viewer — viewport streaming
//
// Fetches a manifest + pre-meshed tile geometry from the tachyne-map pod and
// renders it with three.js. All lighting/tint is baked into per-vertex colors
// by the server-side mesher, so the scene needs no lights: a single
// MeshBasicMaterial with vertexColors multiplies the atlas texture by the
// baked color.
//
// Instead of loading a fixed tile list, the viewer STREAMS tiles around the
// camera (Google-Maps style): the OrbitControls target is the focus point;
// every chunk within LOAD_RADIUS of the focus chunk is fetched (nearest
// first, throttled), and chunks that drift outside LOAD_RADIUS + UNLOAD_MARGIN
// are removed and their geometry disposed. Memory stays bounded at roughly
// (2*LOAD_RADIUS+1)^2 resident tiles no matter how far the user pans.
//
// Server contract (all paths relative to the data base, default the page root):
//   GET manifest.json                    -> { name, dim, spawn:[x,y,z], atlasCell }
//                                           (manifest.tiles is IGNORED — the
//                                           viewer computes which chunks to ask
//                                           for; the pod meshes any chunk on
//                                           demand)
//   GET atlas.png                        -> block-texture atlas, grid of atlasCell-px cells
//   GET tile/{dim}/{cx}/{cz}.json        -> { dim, cx, cz, positions[], uvs[], colors[], indices[] }
//                                           An EMPTY tile (no indices) is valid:
//                                           "loaded, nothing to draw". A 404 is
//                                           treated the same way, not an error.
//
// Tile schema notes:
//   positions — world-space floats, 3 per vertex
//   uvs       — atlas coords in 0..1, V MEASURED FROM THE TOP of the atlas
//               image (hence texture.flipY = false below)
//   colors    — 0..1 RGB, 3 per vertex, baked light+tint included
//   indices   — CCW-wound front faces
//
// Dev mode: append ?data=testdata to point at the synthetic fixtures under
// web/testdata/ (or any other path/origin serving the same layout).

import * as THREE from 'three';
import { OrbitControls } from './vendor/OrbitControls.js';

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

// Chunks within this Chebyshev radius of the focus chunk are kept loaded:
// a (2R+1)^2 square, so 8 -> up to 17x17 = 289 resident tiles.
const LOAD_RADIUS = 8;
// Tiles are only unloaded beyond LOAD_RADIUS + UNLOAD_MARGIN, so a tile at
// the boundary doesn't thrash load/unload as the focus jitters.
const UNLOAD_MARGIN = 2;
// Max tile fetches in flight at once.
const TILE_FETCH_CONCURRENCY = 8;
// Debounce for OrbitControls 'change' -> desired-set recompute (ms).
const STREAM_DEBOUNCE_MS = 150;
// Safety-net recompute interval (ms) in case a change event is missed.
const STREAM_INTERVAL_MS = 500;
const SKY_COLOR = 0x87ceeb; // light sky blue

// Data base path. Default '' = same place the page is served from.
// ?data=testdata (or an absolute URL) redirects all data fetches.
const dataBase = (new URLSearchParams(location.search).get('data') ?? '')
  .replace(/\/+$/, '');
const dataURL = (path) => (dataBase ? `${dataBase}/${path}` : path);

// ---------------------------------------------------------------------------
// DOM / HUD
// ---------------------------------------------------------------------------

const canvas = document.getElementById('view');
const hudEl = document.getElementById('hud');
const statusEl = document.getElementById('status');

function setStatus(text, isError = false) {
  statusEl.textContent = text;
  statusEl.classList.toggle('error', isError);
  statusEl.classList.remove('hidden');
}

function hideStatus() {
  statusEl.classList.add('hidden');
}

function updateHUD() {
  const [fx, fz] = focusChunk();
  hudEl.textContent =
    `tiles  ${stats.resident} resident, ${stats.inFlight} in flight\n` +
    `chunk  ${fx}, ${fz}\n` +
    `height ${camera.position.y.toFixed(1)}\n` +
    `streaming r=${LOAD_RADIUS}${live ? ' · live' : ''}`;
}

// ---------------------------------------------------------------------------
// Renderer / scene / camera
// ---------------------------------------------------------------------------

const renderer = new THREE.WebGLRenderer({ canvas, antialias: true });
renderer.setPixelRatio(Math.min(window.devicePixelRatio, 2));
renderer.setSize(window.innerWidth, window.innerHeight);

const scene = new THREE.Scene();
scene.background = new THREE.Color(SKY_COLOR);
// Light fog fading into the sky color hides the loading horizon.
scene.fog = new THREE.Fog(SKY_COLOR, 300, 1800);

const camera = new THREE.PerspectiveCamera(
  70, window.innerWidth / window.innerHeight, 0.1, 4000);

const controls = new OrbitControls(camera, renderer.domElement);
controls.enableDamping = true;
controls.dampingFactor = 0.1;
controls.maxDistance = 2000;

window.addEventListener('resize', () => {
  camera.aspect = window.innerWidth / window.innerHeight;
  camera.updateProjectionMatrix();
  renderer.setSize(window.innerWidth, window.innerHeight);
});

renderer.setAnimationLoop(() => {
  controls.update();
  updateHUD();
  renderer.render(scene, camera);
});

// ---------------------------------------------------------------------------
// Atlas texture
// ---------------------------------------------------------------------------

function loadAtlas(url) {
  return new Promise((resolve, reject) => {
    new THREE.TextureLoader().load(url, (tex) => {
      // Tile UVs measure V from the TOP of the atlas image; disabling the
      // default Y-flip on upload makes v=0 sample the top row directly.
      tex.flipY = false;
      // Crisp pixels: no filtering, no mipmaps (cells would bleed).
      tex.magFilter = THREE.NearestFilter;
      tex.minFilter = THREE.NearestFilter;
      tex.generateMipmaps = false;
      tex.colorSpace = THREE.SRGBColorSpace;
      resolve(tex);
    }, undefined, () => reject(new Error(`failed to load ${url}`)));
  });
}

// ---------------------------------------------------------------------------
// Tile mesh building
// ---------------------------------------------------------------------------

// Build one BufferGeometry mesh from a Tile JSON payload.
function buildTileMesh(tile, material) {
  const { positions, uvs, colors, indices } = tile;
  const vcount = positions.length / 3;

  // Cheap sanity checks — a malformed tile should be loud, not garbled.
  if (uvs.length !== vcount * 2 || colors.length !== vcount * 3) {
    throw new Error(
      `tile ${tile.cx},${tile.cz}: attribute length mismatch ` +
      `(pos ${positions.length}, uv ${uvs.length}, col ${colors.length})`);
  }
  for (let i = 0; i < indices.length; i++) {
    if (indices[i] >= vcount) {
      throw new Error(`tile ${tile.cx},${tile.cz}: index ${indices[i]} out of range (${vcount} verts)`);
    }
  }

  const geo = new THREE.BufferGeometry();
  geo.setAttribute('position', new THREE.Float32BufferAttribute(positions, 3));
  geo.setAttribute('uv', new THREE.Float32BufferAttribute(uvs, 2));
  geo.setAttribute('color', new THREE.Float32BufferAttribute(colors, 3));
  geo.setIndex(indices); // three picks Uint16/Uint32 storage as needed
  geo.computeBoundingSphere(); // for frustum culling

  return new THREE.Mesh(geo, material);
}

async function fetchTile(dim, cx, cz) {
  const res = await fetch(dataURL(`tile/${dim}/${cx}/${cz}.json`));
  if (!res.ok) throw new Error(`tile ${cx},${cz}: HTTP ${res.status}`);
  return res.json(); // gzip on the wire is transparent to fetch()
}

// ---------------------------------------------------------------------------
// Streaming state
// ---------------------------------------------------------------------------

const LOADING = 'loading'; // sentinel state while a fetch is in flight

// "cx,cz" -> LOADING | THREE.Mesh (drawn) | null (loaded, empty/404).
// A key's presence means "don't fetch this coord again" — until it is
// unloaded for being out of range.
const tiles = new Map();

// Desired-but-not-yet-loading coords, sorted nearest-to-focus first.
// Rebuilt wholesale on every recompute, so stale entries simply vanish.
let pendingQueue = [];

const stats = { resident: 0, inFlight: 0 };

// Set once the manifest + atlas are in; streaming is a no-op before that.
let streaming = null; // { dim, material }

// True while the live-update event stream is connected.
let live = false;

const tileKey = (cx, cz) => `${cx},${cz}`;

// The focus point is the OrbitControls orbit/pan target (on the ground);
// world coords -> chunk coords at 16 blocks per chunk.
function focusChunk() {
  return [Math.floor(controls.target.x / 16), Math.floor(controls.target.z / 16)];
}

// Recompute the desired set around the focus chunk: queue loads for missing
// coords, unload coords beyond the margin. Cheap (~O(radius^2 + resident)),
// safe to call often.
function updateStreaming() {
  if (!streaming) return;
  const [fx, fz] = focusChunk();

  // Unload: anything resident beyond LOAD_RADIUS + UNLOAD_MARGIN (Chebyshev).
  // In-flight fetches are left alone; their completion handler re-checks
  // range and discards out-of-range results itself.
  for (const [key, state] of tiles) {
    if (state === LOADING) continue;
    const [cx, cz] = key.split(',').map(Number);
    if (Math.max(Math.abs(cx - fx), Math.abs(cz - fz)) > LOAD_RADIUS + UNLOAD_MARGIN) {
      unloadTile(key, state);
    }
  }

  // Load: every coord within LOAD_RADIUS not already tracked, nearest first.
  pendingQueue = [];
  for (let dz = -LOAD_RADIUS; dz <= LOAD_RADIUS; dz++) {
    for (let dx = -LOAD_RADIUS; dx <= LOAD_RADIUS; dx++) {
      const cx = fx + dx, cz = fz + dz;
      if (!tiles.has(tileKey(cx, cz))) {
        pendingQueue.push([cx, cz, dx * dx + dz * dz]);
      }
    }
  }
  pendingQueue.sort((a, b) => a[2] - b[2]);
  pumpQueue();
}

// Remove a resident tile: drop the mesh from the scene and free its GPU
// geometry. The shared material and atlas texture are NOT disposed.
function unloadTile(key, state) {
  if (state instanceof THREE.Mesh) {
    scene.remove(state);
    state.geometry.dispose();
  }
  tiles.delete(key);
  stats.resident--;
}

// Start fetches from the pending queue up to the concurrency cap.
function pumpQueue() {
  while (stats.inFlight < TILE_FETCH_CONCURRENCY && pendingQueue.length > 0) {
    const [cx, cz] = pendingQueue.shift();
    const key = tileKey(cx, cz);
    if (tiles.has(key)) continue; // already loading/loaded via an older queue
    tiles.set(key, LOADING);
    stats.inFlight++;
    loadTile(cx, cz, key); // async; completion pumps again
  }
}

async function loadTile(cx, cz, key) {
  let mesh = null; // null = loaded-empty (no geometry, or fetch failed)
  try {
    const tile = await fetchTile(streaming.dim, cx, cz);
    if (tile.indices && tile.indices.length > 0) {
      mesh = buildTileMesh(tile, streaming.material);
    }
  } catch (err) {
    // 404s (unmeshed/out-of-world chunks) and transient errors are expected
    // while panning — record the coord as loaded-empty so it isn't retried
    // every tick, and keep the noise at debug level.
    console.debug(`tile ${cx},${cz} skipped:`, err.message ?? err);
  }
  stats.inFlight--;

  // The focus may have moved while this fetch was in flight; drop results
  // that are already out of keep-range instead of parking them resident.
  const [fx, fz] = focusChunk();
  if (Math.max(Math.abs(cx - fx), Math.abs(cz - fz)) > LOAD_RADIUS + UNLOAD_MARGIN) {
    if (mesh) mesh.geometry.dispose();
    tiles.delete(key);
  } else {
    if (mesh) scene.add(mesh);
    tiles.set(key, mesh);
    stats.resident++;
  }
  pumpQueue();
}

// Debounced recompute on camera movement + a periodic safety net.
let streamDebounce = 0;
controls.addEventListener('change', () => {
  clearTimeout(streamDebounce);
  streamDebounce = setTimeout(updateStreaming, STREAM_DEBOUNCE_MS);
});
setInterval(updateStreaming, STREAM_INTERVAL_MS);

// ---------------------------------------------------------------------------
// Live updates
// ---------------------------------------------------------------------------

// The pod pushes tile invalidations over Server-Sent Events whenever the world
// changes, so builds appear on the map seconds after they are placed.
// EventSource reconnects on its own, so a pod restart or a dropped connection
// heals with no retry logic here.
function connectLiveUpdates() {
  const es = new EventSource(dataURL('events'));
  es.addEventListener('tile', (e) => {
    try {
      const { cx, cz } = JSON.parse(e.data);
      invalidateTile(cx, cz);
    } catch (err) {
      console.debug('live: bad tile event', err);
    }
  });
  es.addEventListener('open', () => { live = true; });
  es.onerror = () => { live = false; }; // EventSource retries on its own
  return es;
}

// Forget a tile so the next streaming pass re-fetches it with fresh geometry.
function invalidateTile(cx, cz) {
  const key = tileKey(cx, cz);
  const state = tiles.get(key);
  if (state === undefined) return; // not resident — nothing to refresh
  if (state === LOADING) return;   // in flight; it will pick up the change
  unloadTile(key, state);
  scheduleLiveRefresh();
}

// One edit dirties a 3x3 of tiles, so coalesce a burst into a single recompute.
let liveDebounce = 0;
function scheduleLiveRefresh() {
  clearTimeout(liveDebounce);
  liveDebounce = setTimeout(updateStreaming, 100);
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

async function main() {
  setStatus('loading manifest…');
  const res = await fetch(dataURL('manifest.json'));
  if (!res.ok) throw new Error(`manifest.json: HTTP ${res.status}`);
  const manifest = await res.json();

  document.title = `tachyne map — ${manifest.name}`;

  // Aim the camera above and back from spawn before anything loads.
  const [sx, sy, sz] = manifest.spawn;
  controls.target.set(sx, sy, sz);
  camera.position.set(sx + 48, sy + 72, sz + 96);
  camera.lookAt(sx, sy, sz);

  setStatus('loading atlas…');
  const atlas = await loadAtlas(dataURL('atlas.png'));

  const material = new THREE.MeshBasicMaterial({
    map: atlas,
    vertexColors: true,     // baked light+tint multiplies the atlas texture
    side: THREE.FrontSide,  // tiles are CCW-wound front faces
    alphaTest: 0.5,         // cutout foliage/glass; no sorting needed
  });

  // Everything is ready — start streaming tiles around the focus point.
  // From here on, tiles load and unload as the camera moves; there is no
  // fixed tile list and no "done loading" moment.
  streaming = { dim: manifest.dim, material };
  hideStatus();
  updateStreaming();

  // Follow world edits so builds appear without a reload.
  connectLiveUpdates();
}

main().catch((err) => {
  console.error(err);
  setStatus(`error: ${err.message}`, true);
});
