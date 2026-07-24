package main

import (
	"encoding/json"
	"log"

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

// followBlockChanges replays the engine's block edits into the map's own
// in-memory world and marks the affected tiles dirty, so the viewer sees
// builds appear within a flush interval.
func (s *server) followBlockChanges(c *busplugin.Conn) error {
	_, err := busplugin.On(c, "block_change", func(ev blockChange) {
		s.applyBlockChange(ev)
	})
	return err
}

// applyBlockChange mutates the map's world view and queues the re-render.
func (s *server) applyBlockChange(ev blockChange) {
	// Note: block_change carries no dimension, so these are treated as
	// overworld edits (which is what the engine publishes for player edits).
	s.reader.SetBlock(ev.X, ev.Y, ev.Z, ev.State)
	s.worldVersion.Add(1)
	s.live.markDirty(ev.X, ev.Z)
}
