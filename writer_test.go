// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tiff

import (
	"bytes"
	"image"
	"io/ioutil"
	"os"
	"testing"
)

var roundtripTests = []struct {
	filename string
	opts     *Options
}{
	{"g4test_1.tiff", nil},
	{"g4test_2.tiff", nil},
	{"video-001.tiff", nil},
	{"video-001-16bit.tiff", nil},
	{"video-001-gray.tiff", nil},
	{"video-001-gray-16bit.tiff", nil},
	{"video-001-paletted.tiff", nil},
	{"bw-packbits.tiff", nil},
	{"video-001.tiff", &Options{Predictor: true}},
	{"video-001.tiff", &Options{Compression: Deflate}},
	{"video-001.tiff", &Options{Predictor: true, Compression: Deflate}},
	{"video-001.tiff", &Options{Compression: LZW}},
	{"video-001.tiff", &Options{Predictor: true, Compression: LZW}},
	{"video-001-16bit.tiff", &Options{Predictor: true, Compression: LZW}},
	{"video-001-gray.tiff", &Options{Predictor: true, Compression: LZW}},
	{"video-001-gray-16bit.tiff", &Options{Predictor: true, Compression: LZW}},
	{"go-aqua-cmyk.tiff", nil},
	{"go-aqua-cmyk.tiff", &Options{Compression: LZW}},
	{"go-aqua-cmyk.tiff", &Options{Predictor: true, Compression: LZW}},
	{"zookeeper-cmyk.tiff", nil},
	{"zookeeper-cmyk.tiff", &Options{Compression: LZW}},
	{"zookeeper-cmyk.tiff", &Options{Predictor: true, Compression: LZW}},
}

func openImage(filename string) (image.Image, error) {
	f, err := os.Open(testdataDir + filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Decode(f)
}

func TestRoundtrip(t *testing.T) {
	for _, rt := range roundtripTests {
		img, err := openImage(rt.filename)
		if err != nil {
			t.Fatal(err)
		}

		out := new(bytes.Buffer)
		err = Encode(out, img, rt.opts)
		if err != nil {
			t.Fatal(err)
		}

		img2, err := Decode(&buffer{buf: out.Bytes()})
		if err != nil {
			t.Fatal(err)
		}
		compare(t, img, img2)
	}
}

// TestRoundtrip2 tests that encoding and decoding an image whose
// origin is not (0, 0) gives the same thing.
func TestRoundtrip2(t *testing.T) {
	m0 := image.NewRGBA(image.Rect(3, 4, 9, 8))
	for i := range m0.Pix {
		m0.Pix[i] = byte(i)
	}
	out := new(bytes.Buffer)
	if err := Encode(out, m0, nil); err != nil {
		t.Fatal(err)
	}
	m1, err := Decode(&buffer{buf: out.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	compare(t, m0, m1)
}

func nextIFDOffset(b []byte, ifdOffset uint32) uint32 {
	n := uint32(enc.Uint16(b[ifdOffset:]))
	return enc.Uint32(b[ifdOffset+2+n*ifdLen:])
}

func TestEncodeAll(t *testing.T) {
	m0 := image.NewGray(image.Rect(0, 0, 4, 3))
	for i := range m0.Pix {
		m0.Pix[i] = byte(i * 11)
	}
	m1 := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for i := range m1.Pix {
		m1.Pix[i] = byte(255 - i*7)
	}

	out := new(bytes.Buffer)
	if err := EncodeAll(out, []image.Image{m0, m1}, nil); err != nil {
		t.Fatal(err)
	}

	b := out.Bytes()
	firstIFDOffset := enc.Uint32(b[4:8])
	secondIFDOffset := nextIFDOffset(b, firstIFDOffset)
	if secondIFDOffset == 0 {
		t.Fatal("second IFD offset = 0, want non-zero")
	}
	if got := nextIFDOffset(b, secondIFDOffset); got != 0 {
		t.Fatalf("third IFD offset = %d, want 0", got)
	}

	got0, err := Decode(&buffer{buf: b})
	if err != nil {
		t.Fatal(err)
	}
	compare(t, m0, got0)

	got1, err := DecodeAt(&buffer{buf: b}, int64(secondIFDOffset))
	if err != nil {
		t.Fatal(err)
	}
	compare(t, m1, got1)
}

func TestEncodeAllNoImages(t *testing.T) {
	if err := EncodeAll(new(bytes.Buffer), nil, nil); err == nil {
		t.Fatal("EncodeAll with no images: got nil error, want non-nil")
	}
}

func benchmarkEncode(b *testing.B, name string, pixelSize int) {
	img, err := openImage(name)
	if err != nil {
		b.Fatal(err)
	}
	s := img.Bounds().Size()
	b.SetBytes(int64(s.X * s.Y * pixelSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Encode(ioutil.Discard, img, nil)
	}
}

func BenchmarkEncode(b *testing.B)         { benchmarkEncode(b, "video-001.tiff", 4) }
func BenchmarkEncodePaletted(b *testing.B) { benchmarkEncode(b, "video-001-paletted.tiff", 1) }
func BenchmarkEncodeGray(b *testing.B)     { benchmarkEncode(b, "video-001-gray.tiff", 1) }
func BenchmarkEncodeGray16(b *testing.B)   { benchmarkEncode(b, "video-001-gray-16bit.tiff", 2) }
func BenchmarkEncodeRGBA(b *testing.B)     { benchmarkEncode(b, "video-001.tiff", 4) }
func BenchmarkEncodeRGBA64(b *testing.B)   { benchmarkEncode(b, "video-001-16bit.tiff", 8) }
