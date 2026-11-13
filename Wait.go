//go:build windows

package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// POST /waitfor: optionally send keys, then watch the screen until a fingerprint matches.
//
// The orchestrator recognises screens by fingerprints: hashes of 32x32 tiles (see
// tileHash). Checking one used to mean shipping a PNG screenshot over the network for every
// look (0.2-0.35 s each). Here the executor sends the keys and re-captures just the area the
// tiles cover every poll_ms, so a screen change is confirmed within a few tens of
// milliseconds of happening, and only a small JSON answer goes back.
//
// Tile keys are "x,y" (the tile's top-left corner) relative to the same area as
// GET /screenshot?scope=all, i.e. the main window plus its dialogs.
//
//	{"keys": ["ctrl+f4"], "delay_ms": 20, "tiles": {"64,96": "ab12cd34ef56ab78"},
//	 "tile": 32, "match": 0.92, "timeout_ms": 4000, "poll_ms": 30}
//	-> {"ok": true, "matched": true, "share": 0.97, "elapsed_ms": 140, "polls": 4,
//	    "width": 962, "height": 1118}
//
// Keys go through the same allowlist, focus and guard checks as POST /keys. Not matching in
// time is a normal answer (matched: false), not an error.
func (s *server) handleWaitFor(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Keys      []string          `json:"keys"`
		DelayMS   *int              `json:"delay_ms"`
		Tiles     map[string]string `json:"tiles"`
		Tile      int               `json:"tile"`
		Match     float64           `json:"match"`
		TimeoutMS int               `json:"timeout_ms"`
		PollMS    int               `json:"poll_ms"`
	}
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	if len(req.Tiles) == 0 {
		return fail(http.StatusBadRequest, "tiles must not be empty")
	}
	if req.Tile <= 0 {
		req.Tile = 32
	}
	if req.Match <= 0 || req.Match > 1 {
		req.Match = 0.92
	}
	timeout := time.Duration(minInt(maxInt(req.TimeoutMS, 0), 30000)) * time.Millisecond
	poll := time.Duration(minInt(maxInt(req.PollMS, 10), 1000)) * time.Millisecond

	type tile struct {
		x, y int
		want string
	}
	tiles := make([]tile, 0, len(req.Tiles))
	minX, minY, maxX, maxY := 1<<30, 1<<30, 0, 0
	for k, v := range req.Tiles {
		parts := strings.Split(k, ",")
		if len(parts) != 2 {
			return fail(http.StatusBadRequest, "bad tile key %q", k)
		}
		x, err1 := strconv.Atoi(parts[0])
		y, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil || x < 0 || y < 0 {
			return fail(http.StatusBadRequest, "bad tile key %q", k)
		}
		tiles = append(tiles, tile{x, y, v})
		minX, minY = minInt(minX, x), minInt(minY, y)
		maxX, maxY = maxInt(maxX, x), maxInt(maxY, y)
	}

	var chords []chord
	if len(req.Keys) > 0 {
		var err error
		if chords, err = s.parseKeys(req.Keys); err != nil {
			return err
		}
	}
	delay := s.cfg.keyDelay
	if req.DelayMS != nil {
		delay = time.Duration(minInt(maxInt(*req.DelayMS, 0), 10000)) * time.Millisecond
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !inputDesktopAvailable() {
		return fail(http.StatusServiceUnavailable, "input desktop unavailable (locked?); a capture would be blank")
	}
	if len(chords) > 0 {
		t, err := s.prepare()
		if err != nil {
			return err
		}
		if err := s.sendChords(chords, delay, s.guard(t.Main.PID)); err != nil {
			s.audit("keys-failed", err.Error(), t.bounds())
			return err
		}
		names := make([]string, len(chords))
		for i, c := range chords {
			names[i] = c.String()
		}
		s.audit("keys", strings.Join(names, " "), t.bounds())
	}

	start := time.Now()
	polls, share, width, height := 0, 0.0, 0, 0
	for {
		if s.paused.Load() {
			return fail(http.StatusLocked, "executor is paused; POST /resume to continue")
		}
		if t, err := s.findTarget(); err == nil {
			rc := t.bounds()
			width, height = rc.W, rc.H
			x1, y1 := minInt(maxX+req.Tile, rc.W), minInt(maxY+req.Tile, rc.H)
			if x1 > minX && y1 > minY {
				// Capture only the area the tiles cover.
				img, err := captureRect(Rect{X: rc.X + minX, Y: rc.Y + minY, W: x1 - minX, H: y1 - minY})
				if err == nil {
					hit := 0
					for _, tl := range tiles {
						if tl.x+req.Tile <= rc.W && tl.y+req.Tile <= rc.H &&
							tileHash(img, tl.x-minX, tl.y-minY, req.Tile) == tl.want {
							hit++
						}
					}
					share = float64(hit) / float64(len(tiles))
				}
			}
		}
		polls++
		if share >= req.Match || time.Since(start) >= timeout {
			break
		}
		time.Sleep(poll)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "matched": share >= req.Match, "share": share,
		"elapsed_ms": time.Since(start).Milliseconds(), "polls": polls,
		"width": width, "height": height,
	})
	return nil
}
