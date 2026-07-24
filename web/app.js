// tachyne-map browser viewer
//
// Fetches a manifest + pre-meshed tile geometry from the tachyne-map pod and
// renders it with three.js. All lighting/tint is baked into per-vertex colors
// by the server-side mesher, so the scene needs no lights: a single
// MeshBasicMaterial with vertexColors multiplies the atlas texture by the
// baked color.
//
// Server contract (all paths relative to the data base, default the page root):
//   GET manifest.json                    -> { name, dim, spawn:[x,y,z], atlasCell, tiles:[[cx,cz],...] }
//   GET atlas.png                        -> block-texture atlas, grid of atlasCell-px cells
//   GET tile/{dim}/{cx}/{cz}.json        -> { dim, cx, cz, positions[], uvs[], colors[], indices[] }
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

const TILE_FETCH_CONCURRENCY = 8;
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

const hud = {
  tilesLoaded: 0,
  tilesTotal: 0,
  tilesFailed: 0,
  loading: true,
};

function setStatus(text, isError = false) {
  statusEl.textContent = text;
  statusEl.classList.toggle('error', isError);
  statusEl.classList.remove('hidden');
}

function hideStatus() {
  statusEl.classList.add('hidden');
}

function updateHUD() {
  const p = camera.position;
  const failed = hud.tilesFailed ? `  (${hud.tilesFailed} failed)` : '';
  hudEl.textContent =
    `tiles  ${hud.tilesLoaded}/${hud.tilesTotal}${failed}\n` +
    `camera ${p.x.toFixed(1)}, ${p.y.toFixed(1)}, ${p.z.toFixed(1)}`;
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
// Tile loading
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

// Run fn over items with at most `limit` in flight at once.
async function forEachLimited(items, limit, fn) {
  let next = 0;
  const worker = async () => {
    while (next < items.length) {
      const item = items[next++];
      await fn(item);
    }
  };
  const n = Math.min(limit, items.length);
  await Promise.all(Array.from({ length: n }, worker));
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

  // Sort tiles nearest-to-spawn first so the area around the camera pops in
  // before the fringes.
  const tiles = [...manifest.tiles].sort((a, b) => {
    const da = (a[0] * 16 + 8 - sx) ** 2 + (a[1] * 16 + 8 - sz) ** 2;
    const db = (b[0] * 16 + 8 - sx) ** 2 + (b[1] * 16 + 8 - sz) ** 2;
    return da - db;
  });

  hud.tilesTotal = tiles.length;
  setStatus(`loading tiles… 0/${tiles.length}`);

  await forEachLimited(tiles, TILE_FETCH_CONCURRENCY, async ([cx, cz]) => {
    try {
      const tile = await fetchTile(manifest.dim, cx, cz);
      scene.add(buildTileMesh(tile, material));
      hud.tilesLoaded++;
    } catch (err) {
      hud.tilesFailed++;
      console.error(err);
    }
    setStatus(`loading tiles… ${hud.tilesLoaded + hud.tilesFailed}/${tiles.length}`);
  });

  hud.loading = false;
  if (hud.tilesFailed > 0) {
    setStatus(`loaded with ${hud.tilesFailed} failed tile(s) — see console`, true);
  } else {
    hideStatus();
  }
}

main().catch((err) => {
  console.error(err);
  setStatus(`error: ${err.message}`, true);
});
