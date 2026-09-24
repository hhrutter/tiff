// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tiff

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"testing"
)

type tileRowCase struct {
	name    string
	mode    imageMode
	bits    uint
	samples int
}

// TestTileRows checks padded rows with colored, partially transparent, and full-precision samples.
func TestTileRows(t *testing.T) {
	cases := []tileRowCase{
		{"gray1", mGray, 1, 1}, {"gray1-invert", mGrayInvert, 1, 1},
		{"gray8", mGray, 8, 1}, {"gray8-invert", mGrayInvert, 8, 1},
		{"gray16", mGray, 16, 1}, {"gray16-invert", mGrayInvert, 16, 1},
		{"paletted1", mPaletted, 1, 1}, {"paletted8", mPaletted, 8, 1},
		{"rgb8", mRGB, 8, 3}, {"rgb16", mRGB, 16, 3},
		{"rgba8", mRGBA, 8, 4}, {"rgba16", mRGBA, 16, 4},
		{"nrgba8", mNRGBA, 8, 4}, {"nrgba16", mNRGBA, 16, 4},
		{"cmyk8", mCMYK, 8, 4},
	}
	for _, tc := range cases {
		for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
			for _, predictor := range []bool{false, true} {
				if predictor && tc.bits == 1 {
					continue
				}
				t.Run(fmt.Sprintf("%s/%s/predictor=%v", tc.name, order, predictor), func(t *testing.T) {
					checkTileRows(t, tc, order, predictor)
				})
			}
		}
	}
}

func tileRowSample(tc tileRowCase, x, y, component int) uint16 {
	if tc.bits == 1 {
		return uint16((x + y) % 2)
	}
	v := uint16((x*1703 + y*2791 + component*8191 + 0x1234) & 0x7fff)
	if component == 3 && tc.mode != mCMYK {
		v = 0xc3a7
	}
	if tc.bits == 8 {
		v >>= 8
	}
	return v
}

func tileRowBuffer(tc tileRowCase, order binary.ByteOrder) []byte {
	rowBytes := (16*tc.samples*int(tc.bits) + 7) / 8
	b := make([]byte, 16*rowBytes)
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			for c := 0; c < tc.samples; c++ {
				v := tileRowSample(tc, x, y, c)
				off := y*rowBytes + (x*tc.samples+c)*int(tc.bits)/8
				switch tc.bits {
				case 1:
					b[off] |= byte(v) << uint(7-x%8)
				case 8:
					b[off] = byte(v)
				case 16:
					order.PutUint16(b[off:], v)
				}
			}
		}
	}
	return b
}

func predictTileRows(b []byte, tc tileRowCase, order binary.ByteOrder) {
	step := int(tc.bits / 8)
	pixelBytes := tc.samples * step
	for y := 0; y < 16; y++ {
		for pos := 16*pixelBytes - step; pos >= pixelBytes; pos -= step {
			off := y*16*pixelBytes + pos
			if step == 1 {
				b[off] -= b[off-pixelBytes]
			} else {
				v := order.Uint16(b[off:]) - order.Uint16(b[off-pixelBytes:])
				order.PutUint16(b[off:], v)
			}
		}
	}
}

func tileRowPalette() color.Palette {
	p := make(color.Palette, 256)
	for i := range p {
		p[i] = color.RGBA{uint8(i), uint8(255 - i), uint8(i ^ 0x55), 255}
	}
	return p
}

func tileRowImage(tc tileRowCase, bounds image.Rectangle) image.Image {
	switch tc.mode {
	case mGray, mGrayInvert:
		if tc.bits == 16 {
			return image.NewGray16(bounds)
		}
		return image.NewGray(bounds)
	case mPaletted:
		return image.NewPaletted(bounds, tileRowPalette())
	case mNRGBA:
		if tc.bits == 16 {
			return image.NewNRGBA64(bounds)
		}
		return image.NewNRGBA(bounds)
	case mCMYK:
		return image.NewCMYK(bounds)
	default:
		if tc.bits == 16 {
			return image.NewRGBA64(bounds)
		}
		return image.NewRGBA(bounds)
	}
}

func tileRowColor(tc tileRowCase, x, y int) color.Color {
	var v [4]uint16
	for c := 0; c < tc.samples; c++ {
		v[c] = tileRowSample(tc, x, y, c)
	}
	if tc.mode == mPaletted {
		return tileRowPalette()[v[0]]
	}
	if tc.mode == mCMYK {
		return color.CMYK{uint8(v[0]), uint8(v[1]), uint8(v[2]), uint8(v[3])}
	}
	for c := 0; c < tc.samples; c++ {
		v[c] = uint16(uint32(v[c]) * 65535 / ((1 << tc.bits) - 1))
	}
	switch tc.mode {
	case mGray:
		return color.Gray16{Y: v[0]}
	case mGrayInvert:
		return color.Gray16{Y: 65535 - v[0]}
	case mNRGBA:
		return color.NRGBA64{R: v[0], G: v[1], B: v[2], A: v[3]}
	case mRGB:
		v[3] = 65535
	}
	return color.RGBA64{R: v[0], G: v[1], B: v[2], A: v[3]}
}

func checkTileRowPixels(t *testing.T, im image.Image, tc tileRowCase, origin image.Point) {
	t.Helper()
	for y := origin.Y; y < im.Bounds().Max.Y; y++ {
		for x := origin.X; x < im.Bounds().Max.X; x++ {
			r, g, b, a := im.At(x, y).RGBA()
			wr, wg, wb, wa := tileRowColor(tc, x-origin.X, y-origin.Y).RGBA()
			if r != wr || g != wg || b != wb || a != wa {
				t.Fatalf("(%d,%d): RGBA = (%d,%d,%d,%d), want (%d,%d,%d,%d)", x, y, r, g, b, a, wr, wg, wb, wa)
			}
		}
	}
}

func checkTileRows(t *testing.T, tc tileRowCase, order binary.ByteOrder, predictor bool) {
	t.Helper()
	for _, origin := range []image.Point{{}, {X: 16, Y: 16}} {
		for _, width := range []int{1, 7, 15, 16} {
			t.Run(fmt.Sprintf("origin=%v/width=%d", origin, width), func(t *testing.T) {
				im := tileRowImage(tc, image.Rect(0, 0, origin.X+width, origin.Y+13))
				d := decoder{
					mode: tc.mode, bpp: tc.bits, byteOrder: order, palette: tileRowPalette(),
					buf: tileRowBuffer(tc, order), features: map[int][]uint{tBitsPerSample: make([]uint, tc.samples)},
				}
				if predictor {
					d.features[tPredictor] = []uint{prHorizontal}
					predictTileRows(d.buf, tc, order)
				}
				if err := d.decode(im, origin.X, origin.Y, origin.X+16, origin.Y+16); err != nil {
					t.Fatal(err)
				}
				checkTileRowPixels(t, im, tc, origin)
			})
		}
	}
}
