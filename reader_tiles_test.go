// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tiff

import (
	"bytes"
	"compress/bzip2"
	"encoding/binary"
	"fmt"
	"image"
	"io"
	"os"
	"testing"
)

// TestTiledPadding is adapted from golang.org/x/image/tiff v0.46.0.
// The fixtures contain a 15x15 black diagonal on white, stored in a 16x16 tile.
func TestTiledPadding(t *testing.T) {
	for _, name := range []string{
		"gray1", "gray1-invert", "gray8", "gray8-invert", "gray16",
		"paletted1", "paletted8", "rgb8", "rgb16", "rgba8", "rgba16", "nrgba8", "nrgba16",
	} {
		for _, width := range []int{7, 15} {
			t.Run(fmt.Sprintf("%s/width=%d", name, width), func(t *testing.T) {
				compressed, err := os.ReadFile("testdata/tiled-" + name + ".tiff.bz2")
				if err != nil {
					t.Fatal(err)
				}
				b, err := io.ReadAll(bzip2.NewReader(bytes.NewReader(compressed)))
				if err != nil {
					t.Fatal(err)
				}
				// Width 7 leaves a whole padding byte in 1-bit rows, unlike width 15.
				setTileFixtureWidth(t, b, width)
				im, err := Decode(bytes.NewReader(b))
				if err != nil {
					t.Fatal(err)
				}
				checkTileDiagonal(t, im, width)
			})
		}
	}
}

func setTileFixtureWidth(t *testing.T, b []byte, width int) {
	t.Helper()
	if string(b[:4]) != leHeader {
		t.Fatal("expected little-endian fixture")
	}
	o := binary.LittleEndian
	off := int(o.Uint32(b[4:8]))
	count := int(o.Uint16(b[off:]))
	for i := 0; i < count; i++ {
		p := b[off+2+12*i:]
		if o.Uint16(p) != tImageWidth {
			continue
		}
		if o.Uint32(p[4:]) != 1 || o.Uint16(p[2:]) != dtLong {
			t.Fatal("expected LONG ImageWidth scalar")
		}
		o.PutUint32(p[8:], uint32(width))
		return
	}
	t.Fatal("missing fixture ImageWidth")
}

func checkTileDiagonal(t *testing.T, im image.Image, width int) {
	t.Helper()
	if want := image.Rect(0, 0, width, 15); im.Bounds() != want {
		t.Fatalf("bounds = %v, want %v", im.Bounds(), want)
	}
	for y := 0; y < 15; y++ {
		for x := 0; x < width; x++ {
			want := uint32(0xffff)
			if x == y {
				want = 0
			}
			r, g, b, a := im.At(x, y).RGBA()
			if r != want || g != want || b != want || a != 0xffff {
				t.Fatalf("(%d,%d): RGBA = (%d,%d,%d,%d), want (%d,%d,%d,65535)", x, y, r, g, b, a, want, want, want)
			}
		}
	}
}
