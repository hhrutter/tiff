// Copyright 2026 The tiff Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tiff

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"io"
	"math"
	"testing"
)

type boundsImage struct {
	r       image.Rectangle
	atCalls int
}

func (m *boundsImage) ColorModel() color.Model {
	return color.RGBAModel
}

func (m *boundsImage) Bounds() image.Rectangle {
	return m.r
}

func (m *boundsImage) At(_, _ int) color.Color {
	m.atCalls++
	return color.RGBA{}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return len(p) - 1, nil
}

type limitedErrorWriter struct {
	remaining int
	err       error
}

func (w *limitedErrorWriter) Write(p []byte) (int, error) {
	if len(p) <= w.remaining {
		w.remaining -= len(p)
		return len(p), nil
	}
	n := w.remaining
	w.remaining = 0
	return n, w.err
}

func encodedIFDEntry(t *testing.T, b []byte, tag uint16) []byte {
	t.Helper()
	if len(b) < 8 {
		t.Fatal("encoded TIFF has no header")
	}
	off := int(enc.Uint32(b[4:8]))
	if off < 0 || off+2 > len(b) {
		t.Fatalf("invalid IFD offset %d", off)
	}
	n := int(enc.Uint16(b[off : off+2]))
	for pos := off + 2; pos+ifdLen <= len(b) && n > 0; pos, n = pos+ifdLen, n-1 {
		if enc.Uint16(b[pos:pos+2]) == tag {
			return b[pos : pos+ifdLen]
		}
	}
	return nil
}

func TestEncodeUsesLongDimensions(t *testing.T) {
	m := &boundsImage{r: image.Rect(0, 0, math.MaxUint16+1, 1)}
	var out bytes.Buffer
	if err := Encode(&out, m, nil); err != nil {
		t.Fatal(err)
	}

	entry := encodedIFDEntry(t, out.Bytes(), tImageWidth)
	if entry == nil {
		t.Fatal("ImageWidth entry is missing")
	}
	if got := enc.Uint16(entry[2:4]); got != dtLong {
		t.Fatalf("ImageWidth datatype = %d, want LONG", got)
	}
	if got := enc.Uint32(entry[8:12]); got != math.MaxUint16+1 {
		t.Fatalf("ImageWidth = %d, want %d", got, math.MaxUint16+1)
	}
	rows := encodedIFDEntry(t, out.Bytes(), tRowsPerStrip)
	if rows == nil {
		t.Fatal("RowsPerStrip entry is missing")
	}
	if got := enc.Uint16(rows[2:4]); got != dtShort {
		t.Fatalf("RowsPerStrip datatype = %d, want SHORT", got)
	}

	cfg, err := DecodeConfig(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != math.MaxUint16+1 || cfg.Height != 1 {
		t.Fatalf("decoded dimensions = %dx%d, want %dx1", cfg.Width, cfg.Height, math.MaxUint16+1)
	}
}

func TestEncodeRejectsInvalidBoundsBeforeReadingPixels(t *testing.T) {
	tests := []image.Rectangle{
		{Min: image.Pt(1, 0), Max: image.Pt(0, 1)},
		image.Rect(0, 0, int(math.MaxUint32/4)+1, 1),
	}
	for _, r := range tests {
		m := &boundsImage{r: r}
		if err := Encode(io.Discard, m, nil); err == nil {
			t.Fatalf("Encode with bounds %v: got nil error", r)
		}
		if m.atCalls != 0 {
			t.Fatalf("Encode with bounds %v made %d At calls", r, m.atCalls)
		}
	}
}

func TestEncodeRejectsInvalidConcreteImageStorage(t *testing.T) {
	tests := []image.Image{
		&image.Gray{Pix: make([]byte, 3), Stride: 2, Rect: image.Rect(0, 0, 2, 2)},
		&image.RGBA{Pix: make([]byte, 8), Stride: 7, Rect: image.Rect(0, 0, 2, 1)},
	}
	for _, m := range tests {
		if err := Encode(io.Discard, m, nil); err == nil {
			t.Fatalf("Encode(%T): got nil error", m)
		}
	}
}

func TestEncodeRejectsTypedNilStandardImage(t *testing.T) {
	var gray *image.Gray
	if err := Encode(io.Discard, gray, nil); err == nil {
		t.Fatal("Encode with typed nil image: got nil error")
	}
}

func TestDeflatePredictorOption(t *testing.T) {
	m := image.NewGray(image.Rect(0, 0, 2, 1))
	for _, tc := range []struct {
		predictor bool
		wantTag   bool
	}{
		{false, false},
		{true, true},
	} {
		var out bytes.Buffer
		opt := &Options{Compression: Deflate, Predictor: tc.predictor}
		if err := Encode(&out, m, opt); err != nil {
			t.Fatal(err)
		}
		if got := encodedIFDEntry(t, out.Bytes(), tPredictor) != nil; got != tc.wantTag {
			t.Fatalf("Predictor tag present = %t, want %t", got, tc.wantTag)
		}
	}
}

func TestEncodeRejectsShortWrite(t *testing.T) {
	err := Encode(shortWriter{}, image.NewGray(image.Rect(0, 0, 1, 1)), nil)
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Encode error = %v, want %v", err, io.ErrShortWrite)
	}
}

func TestEncodePropagatesWriterErrors(t *testing.T) {
	want := errors.New("write failed")
	m := image.NewGray(image.Rect(0, 0, 1, 1))
	for _, limit := range []int{8, 9} {
		w := &limitedErrorWriter{remaining: limit, err: want}
		if err := Encode(w, m, nil); !errors.Is(err, want) {
			t.Fatalf("Encode with write limit %d: error = %v, want %v", limit, err, want)
		}
	}
}

func TestMarshalIFDRejectsInvalidMetadata(t *testing.T) {
	tests := []struct {
		name   string
		offset uint32
		entry  ifdEntry
	}{
		{"datatype", 8, ifdEntry{tImageWidth, len(lengths), []uint32{1}}},
		{"rational", 8, ifdEntry{tXResolution, dtRational, []uint32{72}}},
		{"offset", math.MaxUint32, ifdEntry{tImageWidth, dtLong, []uint32{1}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := marshalIFD(tc.offset, 0, []ifdEntry{tc.entry}); err == nil {
				t.Fatal("marshalIFD: got nil error")
			}
		})
	}
}
