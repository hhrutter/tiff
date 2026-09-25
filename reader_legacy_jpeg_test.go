// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tiff

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"sort"
	"strings"
	"testing"
)

type legacyJPEGField struct {
	tag, kind uint16
	count     uint32
	data      []byte
}

type legacyJPEGSpec struct {
	width, height, blockWidth, blockHeight int
	compression                            uint16
	blocks                                 [][]byte
	tiled, tables, alpha                   bool
}

func legacyJPEGShort(tag uint16, values ...uint16) legacyJPEGField {
	b := make([]byte, 2*len(values))
	for i, v := range values {
		binary.LittleEndian.PutUint16(b[2*i:], v)
	}
	return legacyJPEGField{tag, dtShort, uint32(len(values)), b}
}

func legacyJPEGLong(tag uint16, values ...uint32) legacyJPEGField {
	b := make([]byte, 4*len(values))
	for i, v := range values {
		binary.LittleEndian.PutUint32(b[4*i:], v)
	}
	return legacyJPEGField{tag, dtLong, uint32(len(values)), b}
}

func legacyJPEGFields(s legacyJPEGSpec, offsets, counts []uint32) []legacyJPEGField {
	bits := []uint16{8, 8, 8}
	if s.alpha {
		bits = append(bits, 8)
	}
	f := []legacyJPEGField{
		legacyJPEGLong(tImageWidth, uint32(s.width)), legacyJPEGLong(tImageLength, uint32(s.height)),
		legacyJPEGShort(tBitsPerSample, bits...), legacyJPEGShort(tCompression, s.compression),
		legacyJPEGShort(tPhotometricInterpretation, pRGB), legacyJPEGShort(tSamplesPerPixel, uint16(len(bits))),
	}
	if s.tiled {
		f = append(f, legacyJPEGLong(tTileWidth, uint32(s.blockWidth)),
			legacyJPEGLong(tTileLength, uint32(s.blockHeight)),
			legacyJPEGLong(tTileOffsets, offsets...), legacyJPEGLong(tTileByteCounts, counts...))
	} else {
		f = append(f, legacyJPEGLong(tStripOffsets, offsets...),
			legacyJPEGLong(tRowsPerStrip, uint32(s.blockHeight)), legacyJPEGLong(tStripByteCounts, counts...))
	}
	if s.alpha {
		f = append(f, legacyJPEGShort(tExtraSamples, 1))
	}
	if s.tables {
		// A shared COM marker exercises table insertion without changing the image's coding tables.
		data := []byte{0xff, 0xd8, 0xff, 0xfe, 0, 4, 'o', 'k', 0xff, 0xd9}
		f = append(f, legacyJPEGField{tJPEGTables, 7, uint32(len(data)), data})
	}
	sort.Slice(f, func(i, j int) bool { return f[i].tag < f[j].tag })
	return f
}

func legacyJPEGFixture(t testing.TB, s legacyJPEGSpec) []byte {
	t.Helper()
	across, down := 1, (s.height+s.blockHeight-1)/s.blockHeight
	if s.tiled {
		across = (s.width + s.blockWidth - 1) / s.blockWidth
	}
	if len(s.blocks) != across*down {
		t.Fatalf("got %d JPEG blocks, want %d", len(s.blocks), across*down)
	}
	b := []byte(leHeader + "\x00\x00\x00\x00")
	var offsets, counts []uint32
	for _, block := range s.blocks {
		offsets = append(offsets, uint32(len(b)))
		counts = append(counts, uint32(len(block)))
		b = append(b, block...)
	}
	if len(b)%2 != 0 {
		b = append(b, 0)
	}
	ifd := len(b)
	binary.LittleEndian.PutUint32(b[4:], uint32(ifd))
	fields := legacyJPEGFields(s, offsets, counts)
	b = append(b, make([]byte, 2+12*len(fields)+4)...)
	binary.LittleEndian.PutUint16(b[ifd:], uint16(len(fields)))
	for i, f := range fields {
		p := b[ifd+2+12*i : ifd+2+12*(i+1)]
		binary.LittleEndian.PutUint16(p, f.tag)
		binary.LittleEndian.PutUint16(p[2:], f.kind)
		binary.LittleEndian.PutUint32(p[4:], f.count)
		if len(f.data) <= 4 {
			copy(p[8:], f.data)
			continue
		}
		binary.LittleEndian.PutUint32(p[8:], uint32(len(b)))
		b = append(b, f.data...)
		if len(b)%2 != 0 {
			b = append(b, 0)
		}
	}
	return b
}

func legacyTestJPEG(t testing.TB, width, height int) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			im.SetRGBA(x, y, color.RGBA{80, 160, 240, 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, im, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func legacyJPEGDimensions(t testing.TB, b []byte, width, height uint16) []byte {
	t.Helper()
	b = append([]byte(nil), b...)
	for pos := 2; pos+4 < len(b); {
		marker := b[pos+1]
		if marker == 0xc0 || marker == 0xc1 {
			binary.BigEndian.PutUint16(b[pos+5:], height)
			binary.BigEndian.PutUint16(b[pos+7:], width)
			return b
		}
		if marker == 0xda {
			break
		}
		pos += 2 + int(binary.BigEndian.Uint16(b[pos+2:]))
	}
	t.Fatal("JPEG fixture has no baseline frame")
	return nil
}

func checkLegacyJPEG(t *testing.T, s legacyJPEGSpec, wantError string) {
	t.Helper()
	im, err := Decode(bytes.NewReader(legacyJPEGFixture(t, s)))
	if wantError != "" {
		var formatErr FormatError
		if !errors.As(err, &formatErr) || !strings.Contains(err.Error(), wantError) {
			t.Fatalf("Decode error = %v, want FormatError containing %q", err, wantError)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if im.Bounds() != image.Rect(0, 0, s.width, s.height) {
		t.Fatalf("bounds = %v, want %dx%d", im.Bounds(), s.width, s.height)
	}
	checkLegacyJPEGPixels(t, im, s)
}

func checkLegacyJPEGPixels(t *testing.T, im image.Image, s legacyJPEGSpec) {
	t.Helper()
	for y := 0; y < s.height; y++ {
		for x := 0; x < s.width; x++ {
			r, g, b, a := im.At(x, y).RGBA()
			if a != 0xffff {
				t.Fatalf("(%d,%d): alpha = %d, want 65535", x, y, a)
			}
			if !s.alpha && (r>>8 < 76 || r>>8 > 84 || g>>8 < 156 || g>>8 > 164 || b>>8 < 236 || b>>8 > 244) {
				t.Fatalf("(%d,%d): color = (%d,%d,%d), want near (80,160,240)", x, y, r>>8, g>>8, b>>8)
			}
		}
	}
}

// TestLegacyJPEGDimensions checks complete, prepared, strip, and edge-tile JPEG blocks.
func TestLegacyJPEGDimensions(t *testing.T) {
	j4 := legacyTestJPEG(t, 4, 4)
	for _, mode := range []struct {
		name        string
		compression uint16
		tables      bool
	}{
		{"old", cJPEGOld, false},
		{"new_without_tables", cJPEG, false},
		{"new_with_tables", cJPEG, true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			s := legacyJPEGSpec{width: 4, height: 4, blockWidth: 4, blockHeight: 4,
				compression: mode.compression, blocks: [][]byte{j4}, tables: mode.tables}
			checkLegacyJPEG(t, s, "")
			s.blockHeight = 8 // A single strip may declare more rows than ImageLength.
			checkLegacyJPEG(t, s, "")
			s.blockHeight = 4
			s.width, s.height = 2, 8 // Same pixel count, wrong width and height.
			s.blockWidth, s.blockHeight = 2, 8
			checkLegacyJPEG(t, s, "do not match")
			s.width, s.height = 4, 4
			s.blockWidth, s.blockHeight = 4, 4
			s.blocks[0] = legacyTestJPEG(t, 3, 4)
			checkLegacyJPEG(t, s, "do not match")
			s.blocks[0] = legacyTestJPEG(t, 4, 3)
			checkLegacyJPEG(t, s, "do not match")
			s.blocks[0] = legacyJPEGDimensions(t, j4, 65535, 65535)
			checkLegacyJPEG(t, s, "exceed")
		})
	}
}

type legacyJPEGTrackedReader struct {
	*bytes.Reader
	jpegEnd  int64
	jpegRead int
}

func (r *legacyJPEGTrackedReader) ReadAt(p []byte, off int64) (int, error) {
	if off >= 8 && off < r.jpegEnd {
		r.jpegRead += len(p)
	}
	return r.Reader.ReadAt(p, off)
}

// TestLegacyJPEGConfigStreams ensures rejected unprepared blocks do not read their full compressed segment.
func TestLegacyJPEGConfigStreams(t *testing.T) {
	for _, compression := range []uint16{cJPEGOld, cJPEG} {
		t.Run(fmt.Sprintf("compression=%d", compression), func(t *testing.T) {
			payload := append(legacyTestJPEG(t, 3, 4), make([]byte, 1<<20)...)
			s := legacyJPEGSpec{width: 4, height: 4, blockWidth: 4, blockHeight: 4,
				compression: compression, blocks: [][]byte{payload}}
			b := legacyJPEGFixture(t, s)
			r := &legacyJPEGTrackedReader{Reader: bytes.NewReader(b), jpegEnd: int64(8 + len(payload))}
			_, err := Decode(r)
			var formatErr FormatError
			if !errors.As(err, &formatErr) {
				t.Fatalf("Decode error = %v, want JPEG dimension error", err)
			}
			if r.jpegRead >= 64<<10 {
				t.Fatalf("read %d bytes of a rejected %d-byte JPEG segment", r.jpegRead, len(payload))
			}
		})
	}
}

// TestLegacyJPEGEdges checks the short final strip and padded tile frames.
func TestLegacyJPEGEdges(t *testing.T) {
	for _, compression := range []uint16{cJPEGOld, cJPEG} {
		t.Run(fmt.Sprintf("compression=%d", compression), func(t *testing.T) {
			j4 := legacyTestJPEG(t, 4, 4)
			strip := legacyJPEGSpec{width: 4, height: 6, blockWidth: 4, blockHeight: 4,
				compression: compression, blocks: [][]byte{j4, legacyTestJPEG(t, 4, 2)}}
			checkLegacyJPEG(t, strip, "")
			strip.blocks[1] = j4
			checkLegacyJPEG(t, strip, "exceed")

			j8 := legacyTestJPEG(t, 8, 8)
			tile := legacyJPEGSpec{width: 9, height: 9, blockWidth: 8, blockHeight: 8,
				compression: compression, tiled: true, blocks: [][]byte{j8, j8, j8, j8}}
			checkLegacyJPEG(t, tile, "")
			tile.blocks[3] = legacyTestJPEG(t, 4, 16) // Same pixel count, wrong tile shape.
			checkLegacyJPEG(t, tile, "do not match")
		})
	}
}

// TestLegacyJPEGAPP14 checks the configured dimensions after Adobe marker insertion.
func TestLegacyJPEGAPP14(t *testing.T) {
	payload, err := os.ReadFile("testdata/legacy-cmyk.jpg")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []struct {
		compression uint16
		tables      bool
	}{{cJPEGOld, false}, {cJPEG, false}, {cJPEG, true}} {
		t.Run(fmt.Sprintf("compression=%d/tables=%t", mode.compression, mode.tables), func(t *testing.T) {
			s := legacyJPEGSpec{width: 150, height: 103, blockWidth: 150, blockHeight: 103,
				compression: mode.compression, blocks: [][]byte{payload}, tables: mode.tables, alpha: true}
			checkLegacyJPEG(t, s, "")
			s.width, s.height = 103, 150 // Same pixel count, wrong shape.
			s.blockWidth, s.blockHeight = 103, 150
			checkLegacyJPEG(t, s, "do not match")
			s.width, s.height, s.blockWidth, s.blockHeight = 150, 103, 150, 103
			s.blocks[0] = legacyJPEGDimensions(t, payload, 65535, 65535)
			checkLegacyJPEG(t, s, "exceed")
		})
	}
}
