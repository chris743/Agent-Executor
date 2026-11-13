//go:build windows

package main

import (
	"fmt"
	"image"
	"unsafe"
)

const (
	srcCopy    = 0x00CC0020
	captureBlt = 0x40000000 // include layered windows (tooltips, some dialogs)
	maxPixels  = 8192 * 8192
)

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type bitmapInfo struct {
	Header bitmapInfoHeader
	Colors [1]uint32
}

// captureRect grabs what is actually visible on screen in rc. Anything covering
// the ERP window will be in the image, which is why callers focus it first.
//
// It blits straight into a 32-bit top-down DIB section and reads the pixels from
// its memory, which avoids GetDIBits (unreliable on some RDP/virtual displays).
func captureRect(rc Rect) (*image.RGBA, error) {
	if rc.W <= 0 || rc.H <= 0 {
		return nil, fmt.Errorf("empty capture area %+v", rc)
	}
	if rc.W*rc.H > maxPixels {
		return nil, fmt.Errorf("capture area too large %+v", rc)
	}

	hdc, _, err := pGetDC.Call(0)
	if hdc == 0 {
		return nil, fmt.Errorf("GetDC failed: %v", err)
	}
	defer pReleaseDC.Call(0, hdc)

	mem, _, err := pCreateCompatibleDC.Call(hdc)
	if mem == 0 {
		return nil, fmt.Errorf("CreateCompatibleDC failed: %v", err)
	}
	defer pDeleteDC.Call(mem)

	bi := bitmapInfo{Header: bitmapInfoHeader{
		Size:     uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		Width:    int32(rc.W),
		Height:   -int32(rc.H), // negative = top-down rows
		Planes:   1,
		BitCount: 32,
	}}
	var bits uintptr
	bmp, _, err := pCreateDIBSection.Call(hdc, uintptr(unsafe.Pointer(&bi)), 0,
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	if bmp == 0 || bits == 0 {
		return nil, fmt.Errorf("CreateDIBSection failed: %v", err)
	}
	defer pDeleteObject.Call(bmp)

	old, _, _ := pSelectObject.Call(mem, bmp)
	defer pSelectObject.Call(mem, old)

	ok, _, err := pBitBlt.Call(mem, 0, 0, uintptr(rc.W), uintptr(rc.H),
		hdc, uintptr(rc.X), uintptr(rc.Y), srcCopy|captureBlt)
	if ok == 0 {
		return nil, fmt.Errorf("BitBlt failed (workstation locked, or the client session minimized/disconnected?): %v", err)
	}
	pGdiFlush.Call() // make sure GDI has finished writing into the DIB

	src := unsafe.Slice((*byte)(unsafe.Pointer(bits)), rc.W*rc.H*4)
	img := image.NewRGBA(image.Rect(0, 0, rc.W, rc.H))
	for i := 0; i < len(src); i += 4 { // BGRA -> RGBA
		img.Pix[i] = src[i+2]
		img.Pix[i+1] = src[i+1]
		img.Pix[i+2] = src[i]
		img.Pix[i+3] = 255
	}
	return img, nil
}