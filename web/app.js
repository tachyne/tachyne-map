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
//   GET players.json                     -> { players: [{ eid, name, x, y, z, … }] }
//                                           full snapshot of online players,
//                                           polled every second (absent/404 is
//                                           fine: no markers)
//   GET mobs.json                        -> { mobs: [{ eid, type, x, y, z,
//                                           health, max_health, category }] }
//                                           full snapshot of live mobs,
//                                           polled every 2 s (absent/404 is
//                                           fine: no markers); category is
//                                           "hostile" | "passive" | "other"
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
const LOAD_RADIUS = 20;
// Tiles are only unloaded beyond LOAD_RADIUS + UNLOAD_MARGIN, so a tile at
// the boundary doesn't thrash load/unload as the focus jitters — and so a
// tile you just looked at is still there when you pan back.
const UNLOAD_MARGIN = 4;
// Max tile fetches in flight at once.
const TILE_FETCH_CONCURRENCY = 8;
// Debounce for OrbitControls 'change' -> desired-set recompute (ms).
const STREAM_DEBOUNCE_MS = 150;
// Safety-net recompute interval (ms) in case a change event is missed.
const STREAM_INTERVAL_MS = 500;
// Poll interval for live player positions (ms).
const PLAYER_POLL_MS = 1000;
// Poll interval for live mob positions (ms) — mobs are many, so poll slower.
const MOB_POLL_MS = 2000;
const SKY_COLOR = 0x87ceeb; // light sky blue

// The toggleable marker layers, in panel order. Each colour is the single
// source of truth for BOTH the marker material and its legend swatch, so the
// panel can never claim a colour the map doesn't draw. Players are drawn in a
// per-player colour, so their legend glyph uses a neutral white.
const LAYER_KEYS = ['players', 'names', 'hostile', 'passive', 'other'];
const LAYER_COLORS = {
  players: 0xffffff, // legend only; actual dots use playerColor(eid)
  names: 0xffffff,   // the name pills above players
  hostile: 0xe0483a, // red
  passive: 0x5fbf5f, // green
  other: 0xd9c25a,   // muted yellow
};

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

// ---------------------------------------------------------------------------
// Marker layer toggles
// ---------------------------------------------------------------------------

// Which marker layers are drawn. Hiding a layer is not merely cosmetic: the
// per-frame easing pass skips hidden layers entirely, so a hidden layer costs
// nothing per frame beyond its (still running) poll.
const layerVisible = Object.fromEntries(LAYER_KEYS.map((k) => [k, true]));

// key -> { button, countEl }, populated once at startup.
const layerControls = new Map();

function initLayerPanel() {
  for (const button of document.querySelectorAll('#layers button[data-layer]')) {
    const key = button.dataset.layer;
    if (!(key in layerVisible)) continue; // markup/JS drift — ignore, don't throw
    button.style.setProperty('--layer-color',
      `#${LAYER_COLORS[key].toString(16).padStart(6, '0')}`);
    button.addEventListener('click', () => setLayerVisible(key, !layerVisible[key]));
    layerControls.set(key, { button, countEl: button.querySelector('.count') });
  }
  refreshLayerPanel();
}

function setLayerVisible(key, on) {
  if (layerVisible[key] === on) return;
  layerVisible[key] = on;
  applyLayerVisibility(key);
  refreshLayerPanel();
}

// Push a layer's visibility onto the scene objects it owns.
function applyLayerVisibility(key) {
  const visible = layerVisible[key];
  if (key === 'players') {
    for (const m of playerMarkers.values()) m.group.visible = visible;
    return;
  }
  if (key === 'names') {
    for (const m of playerMarkers.values()) m.label.visible = visible;
    return;
  }
  const layer = mobLayers[key];
  if (layer?.mesh) layer.mesh.visible = visible;
}

// Reflect current state + counts in the panel. Cheap; called on every poll.
function refreshLayerPanel() {
  for (const [key, { button, countEl }] of layerControls) {
    button.setAttribute('aria-pressed', String(layerVisible[key]));
    let text = '';
    if (key === 'players') {
      text = String(playerMarkers.size);
    } else if (key !== 'names') { // a names count would just restate players
      text = String(mobLayers[key]?.entries.length ?? 0);
    }
    if (countEl.textContent !== text) countEl.textContent = text;
  }
}

function updateHUD() {
  const [fx, fz] = focusChunk();
  hudEl.textContent =
    `tiles  ${stats.resident} resident, ${stats.inFlight} in flight\n` +
    `chunk  ${fx}, ${fz}\n` +
    `height ${camera.position.y.toFixed(1)}\n` +
    `players ${playerMarkers.size}\n` +
    `mobs   ${mobCount}\n` +
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
// Fade to sky BEFORE the streamed region ends, so the edge of what's loaded is
// never a visible cut — distance haze instead of a hole. Derived from
// LOAD_RADIUS so the two can't drift apart.
scene.fog = new THREE.Fog(SKY_COLOR, LOAD_RADIUS * 16 * 0.55, LOAD_RADIUS * 16 * 0.95);

const camera = new THREE.PerspectiveCamera(
  70, window.innerWidth / window.innerHeight, 0.1, 4000);

const controls = new OrbitControls(camera, renderer.domElement);
controls.enableDamping = true;
controls.dampingFactor = 0.1;
// Keep the camera inside the streamed region: zooming out past the loaded
// ring is what makes the world look like it's disappearing at the edges.
// LOAD_RADIUS chunks * 16 blocks, with margin for the viewing angle.
controls.maxDistance = LOAD_RADIUS * 16 * 1.2;

window.addEventListener('resize', () => {
  camera.aspect = window.innerWidth / window.innerHeight;
  camera.updateProjectionMatrix();
  renderer.setSize(window.innerWidth, window.innerHeight);
});

renderer.setAnimationLoop(() => {
  controls.update();
  updatePlayerMarkers(); // ease markers toward their latest polled position
  updateMobMarkers();    // same easing, batched through instance matrices
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
      // Crisp up close, mipmapped at distance. magFilter stays Nearest so
      // blocks keep their pixel-art edges when magnified; minFilter blends
      // BETWEEN mip levels but samples nearest WITHIN one, so distant terrain
      // stops aliasing without the texels themselves going soft.
      //
      // Minifying with Nearest was what made the map look grainy: at map
      // distances one screen pixel covers many texels, so it picked a single
      // arbitrary one and the choice churned as the camera moved. Mipmapping
      // is only safe because the atlas pads every cell with an edge-extended
      // gutter (render/atlas.go) — coarse levels would otherwise average
      // neighbouring sprites together.
      tex.magFilter = THREE.NearestFilter;
      tex.minFilter = THREE.NearestMipMapLinearFilter;
      tex.generateMipmaps = true;
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

// Re-fetches for tiles that are ALREADY resident and whose geometry a world
// edit has invalidated. Kept separate from pendingQueue for two reasons: these
// are serviced first (an edit you just made should appear promptly, ahead of
// terrain streaming in from the horizon), and unlike a pending load the tile
// stays in the scene until its replacement arrives.
let refreshQueue = [];

// Keys with a refresh queued or in flight, so a burst of edits in one chunk
// coalesces into a single re-fetch.
const staleTiles = new Set();

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
  staleTiles.delete(key); // a queued refresh for it is now moot
  stats.resident--;
}

// Start fetches from the queues up to the concurrency cap. Refreshes go first:
// they are for tiles the user is looking at right now.
function pumpQueue() {
  while (stats.inFlight < TILE_FETCH_CONCURRENCY && refreshQueue.length > 0) {
    const [cx, cz, key, expected] = refreshQueue.shift();
    if (!tiles.has(key)) {      // unloaded (panned away) since it was queued
      staleTiles.delete(key);
      continue;
    }
    stats.inFlight++;
    refreshTile(cx, cz, key, expected); // async; completion pumps again
  }
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

// Re-fetch a tile that is already on screen, swapping its geometry only once
// the replacement is in hand.
//
// The old mesh stays in the scene for the whole round trip and is removed in
// the SAME operation that adds the new one, so there is never a frame with
// neither. Dropping it up front instead — which is what "invalidate, then let
// the streaming pass reload it" did — opened a hole for a network round trip
// plus a server re-mesh, and since one edit dirties a 3x3 of chunks that hole
// was a 48x48 block area blinking out around the player on every block placed.
// `expected` is the tile state observed when the refresh was queued. If the
// tile has changed identity since — unloaded by a pan, or unloaded and then
// reloaded by the streaming pass — this fetch is stale and must not overwrite
// the newer geometry.
async function refreshTile(cx, cz, key, expected) {
  let mesh = null; // null = the chunk is now empty (all air), a valid result
  try {
    const tile = await fetchTile(streaming.dim, cx, cz);
    if (tile.indices && tile.indices.length > 0) {
      mesh = buildTileMesh(tile, streaming.material);
    }
  } catch (err) {
    // Keep whatever is on screen: a failed refresh should leave the last good
    // geometry alone rather than blank the tile.
    console.debug(`tile ${cx},${cz} refresh skipped:`, err.message ?? err);
    stats.inFlight--;
    staleTiles.delete(key);
    pumpQueue();
    return;
  }
  stats.inFlight--;
  staleTiles.delete(key);

  if (tiles.get(key) !== expected) {
    // Superseded while in flight — drop this result, keep what is on screen.
    if (mesh) mesh.geometry.dispose();
  } else {
    // Atomic swap: the replacement is added BEFORE the old mesh is removed,
    // so the tile is never absent for even one frame.
    if (mesh) scene.add(mesh);
    if (expected instanceof THREE.Mesh) {
      scene.remove(expected);
      expected.geometry.dispose();
    }
    tiles.set(key, mesh); // replaces in place: stats.resident is unchanged
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

// Queue a resident tile for in-place refresh. The tile keeps drawing its
// current geometry until the replacement arrives (see refreshTile), so an
// edit never blanks the area around the player.
function invalidateTile(cx, cz) {
  const key = tileKey(cx, cz);
  const state = tiles.get(key);
  if (state === undefined) return;  // not resident — nothing on screen to refresh
  if (state === LOADING) return;    // initial load in flight; it fetches fresh anyway
  if (staleTiles.has(key)) return;  // already queued or in flight
  staleTiles.add(key);
  refreshQueue.push([cx, cz, key, state]);
  pumpQueue();
}

// ---------------------------------------------------------------------------
// Player markers
// ---------------------------------------------------------------------------

// Live player positions are polled, not pushed: GET players.json returns a
// FULL snapshot { players: [{ eid, name, x, y, z, dim, gamemode, health }] }
// each time — an eid missing from the snapshot has left (or changed
// dimension) and its marker is removed. Coordinates are absolute world
// coords, the same space as tile geometry.
//
// Each player gets a small scene-level group (independent of tiles, so tile
// unloading never touches it): a colored octahedron at the player position
// plus a name label rendered as a Sprite. The label texture is a canvas drawn
// ONCE per player (cached; only rebuilt on a rename) — white text on a dark
// pill. Labels use sizeAttenuation:false (constant screen size at any
// distance) and both parts draw with depthTest:false + a high renderOrder, so
// a player inside a cave is still findable through the terrain.

// How far above the player position the name label is anchored (world units).
const LABEL_LIFT = 2.2;
// Label screen height with sizeAttenuation:false — clip-space-ish units,
// roughly a fraction of the viewport height. Kept small deliberately: the
// label sits directly above the player, which is exactly where a building
// player is placing blocks, so a big pill hides the thing you are watching.
// Press 'n' (or use the layer panel) to hide labels entirely.
const LABEL_SCALE = 0.032;
// Per-frame lerp factor easing a marker toward its latest polled position.
const MARKER_LERP = 0.2;

// eid -> { group, target, name, labelTexture, labelMaterial, dotMaterial }.
const playerMarkers = new Map();

// One geometry shared by every position dot; never disposed per-marker.
const dotGeometry = new THREE.OctahedronGeometry(0.5);

// True while a players.json fetch is in flight — a slow response must not
// stack a second request behind it.
let playersInFlight = false;

// Deterministic per-player dot color: golden-angle hue walk over the eid.
function playerColor(eid) {
  return new THREE.Color().setHSL(((eid * 137.508) % 360) / 360, 0.85, 0.55);
}

// Draw a name once into a canvas: white text on a rounded dark pill.
function makeLabelTexture(name) {
  const font = 'bold 28px system-ui, sans-serif';
  const pad = 12, radius = 10;
  const canvas = document.createElement('canvas');
  let ctx = canvas.getContext('2d');
  ctx.font = font;
  canvas.width = Math.ceil(ctx.measureText(name).width) + pad * 2;
  canvas.height = 28 + pad * 2;
  ctx = canvas.getContext('2d'); // resizing reset the context state

  // Pill background (hand-rolled rounded rect; half-pixel inset keeps the
  // 1px stroke crisp).
  const x = 0.5, y = 0.5, w = canvas.width - 1, h = canvas.height - 1;
  ctx.beginPath();
  ctx.moveTo(x + radius, y);
  ctx.arcTo(x + w, y, x + w, y + h, radius);
  ctx.arcTo(x + w, y + h, x, y + h, radius);
  ctx.arcTo(x, y + h, x, y, radius);
  ctx.arcTo(x, y, x + w, y, radius);
  ctx.closePath();
  ctx.fillStyle = 'rgba(0, 0, 0, 0.65)';
  ctx.fill();
  ctx.strokeStyle = 'rgba(255, 255, 255, 0.35)';
  ctx.stroke();

  ctx.font = font;
  ctx.textAlign = 'center';
  ctx.textBaseline = 'middle';
  ctx.fillStyle = '#fff';
  ctx.fillText(name, canvas.width / 2, canvas.height / 2 + 1);

  const tex = new THREE.CanvasTexture(canvas);
  tex.colorSpace = THREE.SRGBColorSpace;
  // The pill is drawn smaller than its canvas, so it is minified: mipmap it
  // or the text aliases into noise. WebGL2 mipmaps non-power-of-two textures
  // fine, which is why the odd canvas size is no longer a reason to skip them.
  tex.minFilter = THREE.LinearMipmapLinearFilter;
  tex.generateMipmaps = true;
  return tex;
}

// Keep the label's canvas aspect ratio at a constant screen height.
function scaleLabel(label, texture) {
  const { width, height } = texture.image;
  label.scale.set(LABEL_SCALE * (width / height), LABEL_SCALE, 1);
}

function addPlayerMarker(p) {
  const group = new THREE.Group();
  group.position.set(p.x, p.y, p.z); // snap on spawn; lerp only on updates
  group.visible = layerVisible.players; // a marker added while hidden stays hidden

  const dotMaterial = new THREE.MeshBasicMaterial({
    color: playerColor(p.eid),
    depthTest: false, // visible through terrain
  });
  const dot = new THREE.Mesh(dotGeometry, dotMaterial);
  dot.renderOrder = 999; // after all tiles (depthTest is off)

  const name = p.name ?? `eid ${p.eid}`;
  const labelTexture = makeLabelTexture(name);
  const labelMaterial = new THREE.SpriteMaterial({
    map: labelTexture,
    sizeAttenuation: false, // constant screen size at any distance
    depthTest: false,
    transparent: true,
  });
  const label = new THREE.Sprite(labelMaterial);
  scaleLabel(label, labelTexture);
  label.position.y = LABEL_LIFT;
  label.center.set(0.5, 0); // anchor bottom-center: pill grows upward on screen
  label.renderOrder = 1000; // labels above dots
  label.visible = layerVisible.names; // added while names are hidden: stay hidden

  group.add(dot, label);
  scene.add(group);
  playerMarkers.set(p.eid, {
    group,
    label,
    target: new THREE.Vector3(p.x, p.y, p.z),
    name,
    labelTexture,
    labelMaterial,
    dotMaterial,
  });
}

// Drop a marker and free everything it owns (the shared dotGeometry stays).
function removePlayerMarker(eid, m) {
  scene.remove(m.group);
  m.labelMaterial.dispose();
  m.labelTexture.dispose();
  m.dotMaterial.dispose();
  playerMarkers.delete(eid);
}

// Reconcile markers against a snapshot: move/add present eids, remove absent.
function applyPlayers(players) {
  const seen = new Set();
  for (const p of players) {
    if (p == null || typeof p.eid !== 'number' ||
        !Number.isFinite(p.x) || !Number.isFinite(p.y) || !Number.isFinite(p.z)) {
      continue; // malformed entry — skip, never throw
    }
    seen.add(p.eid);
    const m = playerMarkers.get(p.eid);
    if (!m) { addPlayerMarker(p); continue; }
    m.target.set(p.x, p.y, p.z); // render loop lerps toward this
    const name = p.name ?? `eid ${p.eid}`;
    if (name !== m.name) { // rename: rebuild the cached label once
      m.labelTexture.dispose();
      m.labelTexture = makeLabelTexture(name);
      m.labelMaterial.map = m.labelTexture;
      scaleLabel(m.label, m.labelTexture);
      m.name = name;
    }
  }
  for (const [eid, m] of playerMarkers) {
    if (!seen.has(eid)) removePlayerMarker(eid, m);
  }
  refreshLayerPanel();
}

async function pollPlayers() {
  if (playersInFlight) return;
  playersInFlight = true;
  try {
    const res = await fetch(dataURL('players.json'));
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const body = await res.json();
    // players may be [], null, or absent — all mean "nobody online".
    applyPlayers(Array.isArray(body?.players) ? body.players : []);
  } catch (err) {
    // Endpoint absent (old pod, testdata) or transient failure — stay quiet;
    // the next tick simply tries again.
    console.debug('players.json skipped:', err.message ?? err);
  }
  playersInFlight = false;
}

// Per-frame easing toward the latest polled positions (called from the
// animation loop; snapping every second would look jumpy).
function updatePlayerMarkers() {
  for (const m of playerMarkers.values()) {
    m.group.position.lerp(m.target, MARKER_LERP);
  }
}

// ---------------------------------------------------------------------------
// Mob markers
// ---------------------------------------------------------------------------

// Live mob positions come from GET mobs.json — a FULL snapshot
// { mobs: [{ eid, type, x, y, z, health, max_health, category }] } polled
// every MOB_POLL_MS. Unlike players there can be hundreds of mobs, so the
// per-player Group + canvas-label design would drown the renderer in draw
// calls and texture uploads. Instead each category ("hostile" | "passive" |
// "other") is ONE THREE.InstancedMesh sharing a small octahedron geometry and
// a flat-color material: the whole mob population is at most three draw
// calls, and a poll only rewrites instance matrices. No name labels — that is
// exactly the per-mob cost this design exists to avoid.
//
// Capacity: each mesh is allocated at max(256, next power of two >= count)
// instances and only rebuilt (dispose + new InstancedMesh) when a snapshot
// exceeds it; `count` is set to the live number each poll so unused
// instances are never drawn and shrinking snapshots leave no stale ghosts.
//
// Motion: positions ease toward the latest polled target with the same
// per-frame lerp as players — a few hundred Matrix4 translations per frame is
// far cheaper than the draw calls they feed, and snapping every 2 s looks
// terrible for things that walk constantly.
//
// Mobs draw with depthTest:false (findable through terrain, like players) at
// renderOrder BELOW the player dot/label, so players always stay on top.

// Mobs farther than this from the focus point (horizontal distance) are not
// rendered — keeps instance counts sane on a busy world. Matches roughly
// twice the streamed-terrain radius.
const MOB_VIEW_DIST = LOAD_RADIUS * 16 * 2;
// Minimum instance capacity per category mesh.
const MOB_MIN_CAPACITY = 256;
// Below the player dot (999) and label (1000): players draw on top of mobs.
const MOB_RENDER_ORDER = 998;

// Small shared geometry for every mob instance (smaller than the player dot).
const mobGeometry = new THREE.OctahedronGeometry(0.4);

// One layer (material + growable InstancedMesh + this poll's entries) per
// category. An unknown category falls back to "other".
const mobLayers = {
  hostile: makeMobLayer('hostile'),
  passive: makeMobLayer('passive'),
  other: makeMobLayer('other'),
};

// eid -> { cur: Vector3, target: Vector3 }, kept across polls so easing has
// continuity; eids absent from a snapshot are forgotten.
const mobStates = new Map();

let mobCount = 0;        // rendered mobs (post-cull), for the HUD
let mobsInFlight = false;
const mobMatrix = new THREE.Matrix4(); // scratch, reused for every write

function makeMobLayer(key) {
  return {
    key,            // matches the data-layer key on the toggle button
    material: new THREE.MeshBasicMaterial({ color: LAYER_COLORS[key], depthTest: false }),
    mesh: null,     // created lazily / rebuilt on capacity growth
    capacity: 0,
    entries: [],    // mobStates refs for this category, rebuilt each poll
  };
}

// Grow a layer's InstancedMesh to hold at least n instances. Rebuild is rare
// (only when a snapshot exceeds the current power-of-two capacity); the old
// mesh's instance buffers are disposed, the shared geometry/material are not.
function ensureMobCapacity(layer, n) {
  if (n <= layer.capacity) return;
  let cap = Math.max(MOB_MIN_CAPACITY, layer.capacity);
  while (cap < n) cap *= 2;

  if (layer.mesh) {
    scene.remove(layer.mesh);
    layer.mesh.dispose(); // frees instanceMatrix GPU buffer only
  }
  const mesh = new THREE.InstancedMesh(mobGeometry, layer.material, cap);
  mesh.count = 0; // no stale instances drawn before the first matrix write
  mesh.instanceMatrix.setUsage(THREE.DynamicDrawUsage);
  mesh.renderOrder = MOB_RENDER_ORDER;
  mesh.frustumCulled = false; // instance positions aren't in the geometry bounds
  mesh.visible = layerVisible[layer.key];
  scene.add(mesh);
  layer.mesh = mesh;
  layer.capacity = cap;
}

// Reconcile against a snapshot: bucket near-focus mobs into their category
// layer, update easing targets, drop absent eids, and rewrite each layer's
// matrices/count so the draw state is correct even before the next frame.
function applyMobs(mobs) {
  const maxDistSq = MOB_VIEW_DIST * MOB_VIEW_DIST;
  const seen = new Set();
  for (const layer of Object.values(mobLayers)) layer.entries.length = 0;

  for (const mob of mobs) {
    if (mob == null || typeof mob.eid !== 'number' ||
        !Number.isFinite(mob.x) || !Number.isFinite(mob.y) || !Number.isFinite(mob.z)) {
      continue; // malformed entry — skip, never throw
    }
    // Horizontal distance cull around the focus point.
    const dx = mob.x - controls.target.x;
    const dz = mob.z - controls.target.z;
    if (dx * dx + dz * dz > maxDistSq) continue;

    seen.add(mob.eid);
    // Resolve the category layer up front: the hidden-snap below needs to know
    // which layer's visibility applies.
    const layer = mobLayers[mob.category] ?? mobLayers.other;
    let s = mobStates.get(mob.eid);
    if (!s) { // new mob: snap, no fly-in
      s = {
        cur: new THREE.Vector3(mob.x, mob.y, mob.z),
        target: new THREE.Vector3(mob.x, mob.y, mob.z),
      };
      mobStates.set(mob.eid, s);
    } else {
      s.target.set(mob.x, mob.y, mob.z);
      // While hidden the per-frame lerp is skipped; snap so a later toggle
      // shows current positions instead of easing across stale distance.
      if (!layerVisible[layer.key]) s.cur.copy(s.target);
    }
    layer.entries.push(s);
  }

  // Forget mobs absent from the snapshot (died, despawned, or culled away).
  for (const eid of mobStates.keys()) {
    if (!seen.has(eid)) mobStates.delete(eid);
  }
  mobCount = seen.size;

  // Write matrices immediately: a freshly grown mesh must never draw its
  // count with unset (identity) matrices for a frame, and a shrunken count
  // must take effect now, not at the next frame.
  for (const layer of Object.values(mobLayers)) {
    ensureMobCapacity(layer, layer.entries.length);
    const mesh = layer.mesh;
    if (!mesh) continue; // category never had mobs yet
    mesh.count = layer.entries.length;
    for (let i = 0; i < layer.entries.length; i++) {
      const s = layer.entries[i];
      mesh.setMatrixAt(i, mobMatrix.makeTranslation(s.cur.x, s.cur.y, s.cur.z));
    }
    mesh.instanceMatrix.needsUpdate = true;
  }
  refreshLayerPanel();
}

async function pollMobs() {
  if (mobsInFlight) return; // a slow response must not stack a second request
  mobsInFlight = true;
  try {
    const res = await fetch(dataURL('mobs.json'));
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const body = await res.json();
    // mobs may be [], null, or absent — all mean "no mobs".
    applyMobs(Array.isArray(body?.mobs) ? body.mobs : []);
  } catch (err) {
    // Endpoint absent (old pod, testdata) or transient failure — stay quiet;
    // the next tick simply tries again.
    console.debug('mobs.json skipped:', err.message ?? err);
  }
  mobsInFlight = false;
}

// Per-frame easing pass: lerp every visible mob toward its latest polled
// position and rewrite its instance matrix. A few hundred translations per
// frame is cheap; skipped entirely while hidden.
function updateMobMarkers() {
  for (const layer of Object.values(mobLayers)) {
    const mesh = layer.mesh;
    if (!mesh || layer.entries.length === 0) continue;
    if (!layerVisible[layer.key]) continue; // hidden: positions snap on show
    for (let i = 0; i < layer.entries.length; i++) {
      const s = layer.entries[i];
      s.cur.lerp(s.target, MARKER_LERP);
      mesh.setMatrixAt(i, mobMatrix.makeTranslation(s.cur.x, s.cur.y, s.cur.z));
    }
    mesh.instanceMatrix.needsUpdate = true;
  }
}

// Keyboard shortcuts for the layer panel: 'p' toggles players, 'm' toggles all
// three mob categories together (if any is showing, hide them all; otherwise
// show them all). Plain keypresses only — modifier combos like Ctrl+M stay
// with the browser. Both routes go through setLayerVisible, so the panel and
// the scene can never disagree.
const MOB_LAYER_KEYS = LAYER_KEYS.filter((k) => k !== 'players' && k !== 'names');

window.addEventListener('keydown', (e) => {
  if (e.ctrlKey || e.metaKey || e.altKey) return;
  const key = e.key.toLowerCase();
  if (key === 'p') {
    setLayerVisible('players', !layerVisible.players);
  } else if (key === 'n') {
    setLayerVisible('names', !layerVisible.names);
  } else if (key === 'm') {
    const anyShown = MOB_LAYER_KEYS.some((k) => layerVisible[k]);
    for (const k of MOB_LAYER_KEYS) setLayerVisible(k, !anyShown);
  }
});

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
    // Cutout foliage/glass; no sorting needed. Vanilla's 0.1 cutoff, not a
    // half-way one: mipmapping averages alpha as well as colour, so a 0.5
    // threshold would erode thin cutouts (grass blades, leaf gaps) into
    // nothing as they shrink into the coarser levels.
    alphaTest: 0.1,
  });

  // Everything is ready — start streaming tiles around the focus point.
  // From here on, tiles load and unload as the camera moves; there is no
  // fixed tile list and no "done loading" moment.
  streaming = { dim: manifest.dim, material };
  hideStatus();
  updateStreaming();

  // Follow world edits so builds appear without a reload.
  connectLiveUpdates();

  // Marker layer toggles: wire the panel before the first poll so the
  // counts and pressed states are correct from the first snapshot.
  initLayerPanel();

  // Follow players: poll the position snapshot once a second.
  pollPlayers();
  setInterval(pollPlayers, PLAYER_POLL_MS);

  // Follow mobs: slower poll, instanced rendering (see Mob markers above).
  pollMobs();
  setInterval(pollMobs, MOB_POLL_MS);
}

main().catch((err) => {
  console.error(err);
  setStatus(`error: ${err.message}`, true);
});
