package main

import (
	"crypto/sha1"
	"encoding/hex"
	"image"
)

// tileHash fingerprints the n x n tile of img whose top-left corner is (x, y): the first
// 16 hex characters of the SHA-1 of its pixels as RGB bytes, row by row. It must match the
// orchestrator's hash (Pillow: image.crop(box).tobytes() on an RGB image), so fingerprints
// learned from screenshots can be checked here.
func tileHash(img *image.RGBA, x, y, n int) string {
	h := sha1.New()
	row := make([]byte, 0, n*3)
	for yy := y; yy < y+n; yy++ {
		row = row[:0]
		off := img.PixOffset(x, yy)
		for xx := range n {
			p := img.Pix[off+xx*4 : off+xx*4+3]
			row = append(row, p...)
		}
		h.Write(row)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
