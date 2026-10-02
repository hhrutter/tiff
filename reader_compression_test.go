// Copyright 2026 The tiff Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tiff

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"image"
	"os"
	"testing"

	"golang.org/x/image/ccitt"
)

func compressionTestTIFF(t *testing.T, compression uint32, stream []byte) []byte {
	t.Helper()
	ifdOffset := uint32(8 + len(stream))
	ifd, err := marshalIFD(ifdOffset, 0, []ifdEntry{
		{tImageWidth, dtShort, []uint32{1}},
		{tImageLength, dtShort, []uint32{1}},
		{tBitsPerSample, dtShort, []uint32{8}},
		{tCompression, dtShort, []uint32{compression}},
		{tPhotometricInterpretation, dtShort, []uint32{pBlackIsZero}},
		{tStripOffsets, dtLong, []uint32{8}},
		{tSamplesPerPixel, dtShort, []uint32{1}},
		{tRowsPerStrip, dtLong, []uint32{1}},
		{tStripByteCounts, dtLong, []uint32{uint32(len(stream))}},
	})
	if err != nil {
		t.Fatal(err)
	}
	b := binary.LittleEndian.AppendUint16([]byte("II"), 42)
	b = binary.LittleEndian.AppendUint32(b, ifdOffset)
	b = append(b, stream...)
	return append(b, ifd...)
}

func deflateRGBA16TestTIFF(t *testing.T, stream []byte) []byte {
	t.Helper()
	ifdOffset := uint32(8 + len(stream))
	ifd, err := marshalIFD(ifdOffset, 0, []ifdEntry{
		{tImageWidth, dtShort, []uint32{1}},
		{tImageLength, dtShort, []uint32{1}},
		{tBitsPerSample, dtShort, []uint32{16, 16, 16, 16}},
		{tCompression, dtShort, []uint32{cDeflate}},
		{tPhotometricInterpretation, dtShort, []uint32{pRGB}},
		{tStripOffsets, dtLong, []uint32{8}},
		{tSamplesPerPixel, dtShort, []uint32{4}},
		{tRowsPerStrip, dtLong, []uint32{1}},
		{tStripByteCounts, dtLong, []uint32{uint32(len(stream))}},
		{tExtraSamples, dtShort, []uint32{1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	b := binary.LittleEndian.AppendUint16([]byte("II"), 42)
	b = binary.LittleEndian.AppendUint32(b, ifdOffset)
	b = append(b, stream...)
	return append(b, ifd...)
}

func deflateTestData(t *testing.T, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zlib.NewWriter(&b)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func lzwTestData(data []byte) []byte {
	e := newLZWEncoder()
	if len(data) > 0 {
		e.put(int(data[0]))
		for _, b := range data[1:] {
			e.emit(int(b))
		}
	}
	e.put(lzwEOFCode)
	return e.bytes()
}

func TestDecodeIgnoresExcessBlockOutput(t *testing.T) {
	want := byte(0x11)
	tests := []struct {
		name        string
		compression uint32
		stream      []byte
	}{
		{"uncompressed", cNone, []byte{want, 0x22}},
		{"packbits", cPackBits, []byte{1, want, 0x22}},
		{"deflate", cDeflate, deflateTestData(t, []byte{want, 0x22})},
		{"lzw", cLZW, lzwTestData([]byte{want, 0x22})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			img, err := Decode(bytes.NewReader(compressionTestTIFF(t, tt.compression, tt.stream)))
			if err != nil {
				t.Fatal(err)
			}
			gray, ok := img.(*image.Gray)
			if !ok {
				t.Fatalf("image type = %T, want *image.Gray", img)
			}
			if len(gray.Pix) != 1 || gray.Pix[0] != want {
				t.Fatalf("pixels = %x, want %02x", gray.Pix, want)
			}
		})
	}
}

func TestDecodeRejectsDeflateChecksumError(t *testing.T) {
	stream := deflateTestData(t, []byte{0, 1, 2, 3, 4, 5, 6, 7})
	stream[len(stream)-1] ^= 0xff
	if _, err := Decode(bytes.NewReader(deflateRGBA16TestTIFF(t, stream))); err == nil {
		t.Fatal("Decode accepted a bad Deflate checksum")
	}
}

func TestDecodeRejectsShortBlockOutput(t *testing.T) {
	tests := []struct {
		name        string
		compression uint32
		stream      []byte
	}{
		{"uncompressed", cNone, nil},
		{"packbits", cPackBits, nil},
		{"deflate", cDeflate, deflateTestData(t, nil)},
		{"lzw", cLZW, lzwTestData(nil)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Decode(bytes.NewReader(compressionTestTIFF(t, tt.compression, tt.stream))); err == nil {
				t.Fatal("Decode accepted short block output")
			}
		})
	}
}

func TestDecodeRejectsTruncatedFaxBlocks(t *testing.T) {
	for _, name := range []string{"bw-gopher_ccittGroup3.tiff", "bw-gopher_ccittGroup4.tiff"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile("testdata/" + name)
			if err != nil {
				t.Fatal(err)
			}
			data = fuzzPatchIFDValue(data, tStripByteCounts, 1)
			if _, err := Decode(bytes.NewReader(data)); err == nil {
				t.Fatal("Decode accepted a truncated fax strip")
			}
		})
	}
}

func TestCCITTRLEStopsAfterExpectedRows(t *testing.T) {
	data := append(packBits("1011011"), bytes.Repeat([]byte{0xff}, 1<<20)...)
	r := bytes.NewReader(data)
	if _, err := decodeCCITTRLE(r, ccitt.MSB, 8, 1, true, 1); err != nil {
		t.Fatal(err)
	}
	if consumed := len(data) - r.Len(); consumed != 1 {
		t.Fatalf("decodeCCITTRLE consumed %d bytes, want 1", consumed)
	}
}

func TestCCITTRLERejectsOversizedRunImmediately(t *testing.T) {
	_, err := decodeCCITTRLE(bytes.NewReader(packBits("11011")), ccitt.MSB, 8, 1, true, 1)
	if !errors.Is(err, errCCITTRLEOverflow) {
		t.Fatalf("decodeCCITTRLE error = %v, want %v", err, errCCITTRLEOverflow)
	}
}
