package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/tachyne/tachyne-world/busplugin"
)

// Player markers: the map polls the engine's `players` query for a full
// snapshot rather than replaying player_move events. Movement is high
// frequency and a dropped event would leave a marker stranded; a snapshot is
// always self-correcting and costs one small request per interval.

// playerPollInterval is how often the map refreshes player positions.
const playerPollInterval = time.Second

// playerRow is one player in the engine's `players` reply.
type playerRow struct {
	EID      int32   `json:"eid"`
	Name     string  `json:"name"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	Z        float64 `json:"z"`
	Dim      int     `json:"dim"`
	Gamemode int     `json:"gamemode"`
	Health   float32 `json:"health"`
}

type playersReply struct {
	Players []playerRow `json:"players"`
}

// mobRow is one mob in the engine's `mobs` reply, plus a category the viewer
// uses to colour it (the engine reports the type name, not its disposition).
type mobRow struct {
	EID      int32   `json:"eid"`
	Type     string  `json:"type"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	Z        float64 `json:"z"`
	Health   int     `json:"health"`
	Max      int     `json:"max_health"`
	Category string  `json:"category"` // hostile | passive | other
}

type mobsReply struct {
	Mobs []mobRow `json:"mobs"`
}

// playerTracker holds the latest player + mob snapshot for the viewer to draw.
type playerTracker struct {
	mu      sync.RWMutex
	players []playerRow
	mobs    []mobRow
}

func newPlayerTracker() *playerTracker { return &playerTracker{} }

// snapshot returns the players currently in the rendered dimension.
func (t *playerTracker) snapshot() []playerRow {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]playerRow, len(t.players))
	copy(out, t.players)
	return out
}

func (t *playerTracker) set(rows []playerRow) {
	t.mu.Lock()
	t.players = rows
	t.mu.Unlock()
}

// mobSnapshot returns the mobs currently in the rendered dimension.
func (t *playerTracker) mobSnapshot() []mobRow {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]mobRow, len(t.mobs))
	copy(out, t.mobs)
	return out
}

func (t *playerTracker) setMobs(rows []mobRow) {
	t.mu.Lock()
	t.mobs = rows
	t.mu.Unlock()
}

// hostileMobs are the types the viewer draws as threats. The engine's mobs
// query reports a type name but not its disposition, so classify here and keep
// the viewer dumb.
var hostileMobs = map[string]bool{
	"zombie": true, "husk": true, "drowned": true, "zombie_villager": true,
	"skeleton": true, "stray": true, "bogged": true, "wither_skeleton": true,
	"creeper": true, "spider": true, "cave_spider": true, "enderman": true,
	"witch": true, "slime": true, "magma_cube": true, "phantom": true,
	"blaze": true, "ghast": true, "zombified_piglin": true, "piglin": true,
	"piglin_brute": true, "hoglin": true, "zoglin": true, "guardian": true,
	"elder_guardian": true, "shulker": true, "silverfish": true, "endermite": true,
	"vindicator": true, "evoker": true, "pillager": true, "ravager": true,
	"vex": true, "illusioner": true, "warden": true, "breeze": true,
	"ender_dragon": true, "wither": true, "creaking": true,
}

// mobCategory classifies a mob type for marker colouring.
func mobCategory(typ string) string {
	name := trimNS(typ)
	switch {
	case name == "":
		return "other"
	case hostileMobs[name]:
		return "hostile"
	}
	return "passive"
}

// trimNS strips a "minecraft:" namespace prefix if present.
func trimNS(s string) string {
	const ns = "minecraft:"
	if len(s) > len(ns) && s[:len(ns)] == ns {
		return s[len(ns):]
	}
	return s
}

// runPlayerPoll keeps the tracker fresh from the engine. It never fails hard:
// a missed poll (hub busy, bus blip) just leaves the previous snapshot until
// the next tick.
func (t *playerTracker) runPlayerPoll(c *busplugin.Conn) {
	tick := time.NewTicker(playerPollInterval)
	defer tick.Stop()
	warned := false
	n := 0
	for range tick.C {
		var reply playersReply
		if err := c.Request("players", nil, &reply); err != nil {
			if !warned { // don't spam a log line every second while the engine is down
				log.Printf("bus: players query failed (%v) — markers will hold last known positions", err)
				warned = true
			}
			continue
		}
		warned = false
		// Only players in the dimension this map renders.
		rows := reply.Players[:0:0]
		for _, p := range reply.Players {
			if p.Dim == 0 { // overworld
				rows = append(rows, p)
			}
		}
		t.set(rows)

		// Mobs are ambient and far more numerous than players, so refresh them
		// at half the rate. The dim filter keeps nether/End out of the payload.
		if n++; n%2 == 0 {
			t.pollMobs(c)
		}
	}
}

// pollMobs refreshes the mob snapshot (overworld only, categorised).
func (t *playerTracker) pollMobs(c *busplugin.Conn) {
	var reply mobsReply
	if err := c.Request("mobs", map[string]any{"dim": 0}, &reply); err != nil {
		return // keep the previous snapshot; next tick retries
	}
	for i := range reply.Mobs {
		reply.Mobs[i].Category = mobCategory(reply.Mobs[i].Type)
		reply.Mobs[i].Type = trimNS(reply.Mobs[i].Type)
	}
	t.setMobs(reply.Mobs)
}

// handlePlayers serves the current player positions for the viewer's markers.
func (s *server) handlePlayers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(playersReply{Players: s.players.snapshot()})
}

// handleMobs serves the current mob positions for the viewer's markers.
func (s *server) handleMobs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(mobsReply{Mobs: s.players.mobSnapshot()})
}
