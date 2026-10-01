// Copyright 2026 The tiff Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tiff

import (
	"bytes"
	"encoding/binary"
	"image"
	"testing"
)

const (
	lzwClearCode = 256
	lzwEOFCode   = 257
	lzwFirstCode = 258
	lzwMinWidth  = 9
	lzwMaxWidth  = 12
	lzwDictSize  = 1 << lzwMaxWidth
)

// lzwEncoder writes MSB-first TIFF LZW codes, widening one code early as the TIFF variant requires.
type lzwEncoder struct {
	buf   bytes.Buffer
	acc   uint32
	bits  uint
	next  int
	width uint
}

func newLZWEncoder() *lzwEncoder {
	e := &lzwEncoder{next: lzwFirstCode, width: lzwMinWidth}
	e.put(lzwClearCode)
	return e
}

// put writes a code without advancing the dictionary; clear and EOI define no entry.
func (e *lzwEncoder) put(code int) {
	e.acc = e.acc<<e.width | uint32(code)
	e.bits += e.width
	for e.bits >= 8 {
		e.bits -= 8
		e.buf.WriteByte(byte(e.acc >> e.bits))
	}
}

// emit writes a data code and advances the dictionary the way a decoder will.
func (e *lzwEncoder) emit(code int) {
	e.put(code)
	if e.next < lzwDictSize {
		e.next++
	}
	if e.next+1 >= 1<<e.width && e.width < lzwMaxWidth {
		e.width++
	}
}

func (e *lzwEncoder) bytes() []byte {
	if e.bits > 0 {
		e.buf.WriteByte(byte(e.acc << (8 - e.bits)))
		e.bits = 0
	}
	return e.buf.Bytes()
}

func lzwTrailingStream(payload int, trailing func(*lzwEncoder)) ([]byte, []byte) {
	e := newLZWEncoder()
	e.put(0)
	want := []byte{0}
	for i := 1; i < payload; i++ {
		e.emit(i % 256)
		want = append(want, byte(i%256))
	}
	trailing(e)
	return e.bytes(), want
}

func buildSingleStripLZWTIFF(tb testing.TB, width, height int, stream []byte) []byte {
	tb.Helper()

	const tagCount = 9
	const ifdSize = 2 + tagCount*12 + 4

	buf := binary.LittleEndian.AppendUint16([]byte("II"), 42)
	buf = binary.LittleEndian.AppendUint32(buf, 8)
	buf = binary.LittleEndian.AppendUint16(buf, tagCount)
	buf = appendIFDEntry(buf, tImageWidth, dtLong, width)
	buf = appendIFDEntry(buf, tImageLength, dtLong, height)
	buf = appendIFDEntry(buf, tBitsPerSample, dtShort, 8)
	buf = appendIFDEntry(buf, tCompression, dtShort, cLZW)
	buf = appendIFDEntry(buf, tPhotometricInterpretation, dtShort, pBlackIsZero)
	buf = appendIFDEntry(buf, tStripOffsets, dtLong, 8+ifdSize)
	buf = appendIFDEntry(buf, tSamplesPerPixel, dtShort, 1)
	buf = appendIFDEntry(buf, tRowsPerStrip, dtLong, height)
	buf = appendIFDEntry(buf, tStripByteCounts, dtLong, len(stream))
	buf = binary.LittleEndian.AppendUint32(buf, 0)
	return append(buf, stream...)
}

func appendIFDEntry(buf []byte, tag, typ uint16, value int) []byte {
	buf = binary.LittleEndian.AppendUint16(buf, tag)
	buf = binary.LittleEndian.AppendUint16(buf, typ)
	buf = binary.LittleEndian.AppendUint32(buf, 1)
	return binary.LittleEndian.AppendUint32(buf, uint32(value))
}

func assertLZWGrayImage(t *testing.T, data []byte, width, height int, want []byte) {
	t.Helper()

	img, err := Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got, wantBounds := img.Bounds(), image.Rect(0, 0, width, height); got != wantBounds {
		t.Errorf("bounds = %v, want %v", got, wantBounds)
	}
	gray, ok := img.(*image.Gray)
	if !ok {
		t.Fatalf("image type = %T, want *image.Gray", img)
	}
	if !bytes.Equal(gray.Pix, want) {
		t.Fatal("decoded pixels differ")
	}
}

func TestDecodeLZWStripWithoutEOICode(t *testing.T) {
	const width, height = 40, 30

	stream, want := lzwTrailingStream(width*height, func(*lzwEncoder) {})
	data := buildSingleStripLZWTIFF(t, width, height, stream)
	assertLZWGrayImage(t, data, width, height, want)
}

func TestDecodeLZWStripWithTrailingUndefinedCode(t *testing.T) {
	const width, height = 40, 30

	stream, want := lzwTrailingStream(width*height, func(e *lzwEncoder) { e.put(lzwDictSize - 1) })
	data := buildSingleStripLZWTIFF(t, width, height, stream)
	assertLZWGrayImage(t, data, width, height, want)
}

func (e *lzwEncoder) fillToSlot(want int, out []byte) []byte {
	e.put(0)
	out = append(out, 0)
	for i := 1; e.next < want; i++ {
		e.emit(i % 256)
		out = append(out, byte(i%256))
	}
	return out
}

func lzwTableFullStream(tb testing.TB, payload int) ([]byte, []byte) {
	tb.Helper()

	e := newLZWEncoder()
	want := e.fillToSlot(lzwDictSize-2, nil)
	e.emit(lzwFirstCode)
	want = append(want, 0, 1)
	e.emit(lzwDictSize - 1)
	want = append(want, 0, 1, 0)
	for i := 0; len(want) < payload; i++ {
		e.emit(i % 256)
		want = append(want, byte(i%256))
	}
	if len(want) != payload {
		tb.Fatalf("stream decodes to %d bytes, want exactly one strip of %d", len(want), payload)
	}
	e.put(lzwEOFCode)
	return e.bytes(), want
}

func TestDecodeLZWTableFullStrip(t *testing.T) {
	const width, height = 62, 62

	stream, want := lzwTableFullStream(t, width*height)
	data := buildSingleStripLZWTIFF(t, width, height, stream)
	assertLZWGrayImage(t, data, width, height, want)
}

func TestLZWBlockDataSize(t *testing.T) {
	tests := []struct {
		name        string
		width       int
		height      int
		bits        []uint
		limit, want int64
	}{
		{"one_bit_row_padding", 9, 2, []uint{1}, 144, 4},
		{"rgb", 2, 3, []uint{8, 8, 8}, 48, 18},
		{"rgba_16_bit", 2, 3, []uint{16, 16, 16, 16}, 48, 48},
		{"invalid_sample_count", 2, 3, make([]uint, 32), 48, 48},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := decoder{bpp: tt.bits[0], features: map[int][]uint{tBitsPerSample: tt.bits}}
			b := tiffBlock{width: tt.width, height: tt.height}
			if got := d.lzwBlockDataSize(b, tt.limit); got != tt.want {
				t.Errorf("lzwBlockDataSize = %d, want %d", got, tt.want)
			}
		})
	}
}
