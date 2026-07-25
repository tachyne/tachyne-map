package main

import (
	"encoding/json"
	"log"
	"sync"

	"github.com/tachyne/tachyne-world/busplugin"
)

// The map follows the running engine over the NATS bus rather than sharing its
// world files: it asks the engine for the seed at boot (so the map can never
// drift from the world it is rendering) and then replays block-change events
// into its own in-memory copy. Nothing here writes to the engine's storage.

// blockChange is the payload of mc.event.block_change.
//
// `by` is deliberately json.RawMessage: the engine sends an entity id (number)
// for a player edit but the string "world" for a plugin/bus edit, so a typed
// field would fail to unmarshal on every plugin edit. The map doesn't use it.
type blockChange struct {
	X     int             `json:"x"`
	Y     int             `json:"y"`
	Z     int             `json:"z"`
	State uint32          `json:"state"`
	By    json.RawMessage `json:"by"`
}

// worldInfo is the reply data of the mc.cmd.world query.
type worldInfo struct {
	Seed     int64     `json:"seed"`
	Sections int       `json:"sections"`
	Spawn    []float64 `json:"spawn"`
}

// discoverWorld asks the running engine for its world identity. The seed is
// the thing that matters: terrain is a pure function of it, so taking it from
// the engine means the map can't drift from the world the players are in the
// way a duplicated config value would. Returns nil (and the caller falls back
// to its flag) if the bus is unavailable or the engine doesn't answer.
func discoverWorld(c *busplugin.Conn) *worldInfo {
	var info worldInfo
	if err := c.Request("world", nil, &info); err != nil {
		log.Printf("bus: world query failed (%v) — falling back to configured seed", err)
		return nil
	}
	if info.Seed == 0 && info.Sections == 0 {
		log.Printf("bus: world query returned no seed — falling back to configured seed")
		return nil
	}
	return &info
}

// requestSave asks the engine to flush its world file to disk right now.
//
// The map bootstraps by reading that file and then tailing block_change. The
// engine only autosaves every 30s, so without this the snapshot is up to a
// full interval stale AND those edits never arrive on the event stream (they
// happened before we subscribed) — they stay missing until the next restart.
// Someone building quickly loses a visible chunk of their work from the map.
//
// A pre-save engine answers "unknown command", which is not fatal: the map
// then behaves exactly as it did before.
func requestSave(c *busplugin.Conn) {
	var out struct {
		Edits int `json:"edits"`
	}
	if err := c.Request("save", nil, &out); err != nil {
		log.Printf("bus: save request failed (%v) — snapshot may miss up to one autosave interval", err)
		return
	}
	log.Printf("bus: engine flushed the world file (%d block edits) before the snapshot read", out.Edits)
}

// bootFollower replays the engine's block edits into the map's own in-memory
// world (marking the affected tiles dirty, so the viewer sees builds appear
// within a flush interval) and subscribes BEFORE the world snapshot has been
// read, buffering what arrives until a server exists to apply it to.
//
// Order matters: subscribe, then ask for a save, then read the snapshot. Any
// edit made during that sequence lands in the buffer rather than falling into
// the gap between "not yet in the file" and "not yet subscribed".
type bootFollower struct {
	mu  sync.Mutex
	buf []blockChange
	srv *server // nil until attach; while nil, events are buffered
}

func followFromBoot(c *busplugin.Conn) (*bootFollower, error) {
	f := &bootFollower{}
	if _, err := busplugin.On(c, "block_change", f.handle); err != nil {
		return nil, err
	}
	return f, nil
}

// handle buffers an edit while the snapshot is still loading, or applies it
// directly once attached.
func (f *bootFollower) handle(ev blockChange) {
	f.mu.Lock()
	if f.srv == nil {
		f.buf = append(f.buf, ev)
		f.mu.Unlock()
		return
	}
	srv := f.srv
	f.mu.Unlock()
	srv.applyBlockChange(ev)
}

// attach drains everything buffered during bootstrap into srv and switches to
// direct application. The lock is held across the drain so an event arriving
// mid-attach queues behind it and can never be applied out of order.
func (f *bootFollower) attach(srv *server) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ev := range f.buf {
		srv.applyBlockChange(ev)
	}
	n := len(f.buf)
	f.buf = nil
	f.srv = srv
	return n
}

// applyBlockChange mutates the map's world view and queues the re-render.
func (s *server) applyBlockChange(ev blockChange) {
	// Note: block_change carries no dimension, so these are treated as
	// overworld edits (which is what the engine publishes for player edits).
	s.reader.SetBlock(ev.X, ev.Y, ev.Z, ev.State)
	s.worldVersion.Add(1)
	s.live.markDirty(ev.X, ev.Z)
}
