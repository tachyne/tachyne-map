// tachyne-map tile worker: fetch a tile, parse it and build its typed
// arrays off the main thread.
//
// A tile is a few hundred KB of JSON. Parsing it, copying its plain arrays
// into typed arrays and checking its indices on the page's own thread took
// long enough, with hundreds of tiles streaming in around the camera, to
// stall frames — and with them the pan and zoom. Here that work happens on
// a worker; the finished buffers are transferred back without a copy.
//
// Message in:  { id, url }
// Message out: { id, ok: true, tile: { cx, cz, positions, uvs, colors, indices } }
//              { id, ok: false, status, error }

self.onmessage = async (e) => {
  const { id, url } = e.data;
  try {
    const res = await fetch(url);
    if (!res.ok) {
      self.postMessage({ id, ok: false, status: res.status, error: `HTTP ${res.status}` });
      return;
    }
    const t = await res.json();
    if (!t.indices || t.indices.length === 0) {
      self.postMessage({ id, ok: true, tile: { cx: t.cx, cz: t.cz, empty: true } });
      return;
    }
    const vcount = t.positions.length / 3;
    if (t.uvs.length !== vcount * 2 || t.colors.length !== vcount * 3) {
      throw new Error(`attribute length mismatch (pos ${t.positions.length}, uv ${t.uvs.length}, col ${t.colors.length})`);
    }
    const positions = new Float32Array(t.positions);
    const uvs = new Float32Array(t.uvs);
    const colors = new Float32Array(t.colors);
    const indices = vcount > 65535 ? new Uint32Array(t.indices) : new Uint16Array(t.indices);
    for (let i = 0; i < indices.length; i++) {
      if (indices[i] >= vcount) throw new Error(`index ${indices[i]} out of range (${vcount} verts)`);
    }
    self.postMessage(
      { id, ok: true, tile: { cx: t.cx, cz: t.cz, positions, uvs, colors, indices } },
      [positions.buffer, uvs.buffer, colors.buffer, indices.buffer]);
  } catch (err) {
    self.postMessage({ id, ok: false, error: String(err && err.message || err) });
  }
};
