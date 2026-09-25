// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tiff

import (
	"bytes"
	"encoding/binary"
	"image"
	"io"
	"os"
	"testing"
)

const (
	fuzzMaxInput     = 64 << 10
	fuzzMaxDimension = 512
	fuzzMaxPixels    = 1 << 16
	fuzzMaxOffset    = 1 << 40
)

func fuzzFixture(f *testing.F, name string) []byte {
	f.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		f.Fatal(err)
	}
	if len(b) > fuzzMaxInput {
		f.Fatalf("fixture %s exceeds fuzz input limit", name)
	}
	return b
}

func fuzzPatchIFDValue(b []byte, tag uint16, value uint32) []byte {
	b = append([]byte(nil), b...)
	if len(b) < 8 || !bytes.Equal(b[:2], []byte("II")) {
		return b
	}
	ifd := uint64(binary.LittleEndian.Uint32(b[4:8]))
	if ifd+2 > uint64(len(b)) {
		return b
	}
	n := uint64(binary.LittleEndian.Uint16(b[ifd:]))
	for i := uint64(0); i < n; i++ {
		p := ifd + 2 + i*12
		if p+12 > uint64(len(b)) {
			break
		}
		if binary.LittleEndian.Uint16(b[p:]) == tag {
			binary.LittleEndian.PutUint32(b[p+8:], value)
			break
		}
	}
	return b
}

func fuzzMultiPage(f *testing.F) ([]byte, int64) {
	f.Helper()
	var out bytes.Buffer
	images := []image.Image{image.NewGray(image.Rect(0, 0, 2, 2)), image.NewRGBA(image.Rect(0, 0, 2, 2))}
	if err := EncodeAll(&out, images, nil); err != nil {
		f.Fatal(err)
	}
	b := out.Bytes()
	second := nextIFDOffset(b, binary.LittleEndian.Uint32(b[4:8]))
	if second == 0 {
		f.Fatal("missing second IFD")
	}
	return b, int64(second)
}

func fuzzTIFFSeeds(f *testing.F) ([][]byte, int64) {
	f.Helper()
	seeds := [][]byte{nil, {0}, []byte(leHeader), []byte(leHeader + "\xff\xff\xff\x7f")}
	for _, name := range []string{
		"bw-uncompressed.tiff", "bw-packbits.tiff", "bw-deflate.tiff",
		"bw-gopher_ccittGroup3.tiff", "bw-gopher_ccittGroup4.tiff",
		"blue-purple-pink.lzwcompressed.tiff", "video-001-16bit.tiff",
		"video-001-tile-64x64.tiff",
	} {
		seeds = append(seeds, fuzzFixture(f, name))
	}

	j4 := legacyTestJPEG(f, 4, 4)
	for _, compression := range []uint16{cJPEGOld, cJPEG} {
		s := legacyJPEGSpec{width: 4, height: 4, blockWidth: 4, blockHeight: 4, compression: compression, blocks: [][]byte{j4}}
		seeds = append(seeds, legacyJPEGFixture(f, s))
		s.blocks[0] = j4[:len(j4)/2]
		seeds = append(seeds, legacyJPEGFixture(f, s))
		s.blocks[0] = legacyJPEGDimensions(f, j4, 65535, 65535)
		seeds = append(seeds, legacyJPEGFixture(f, s))
	}

	strip := legacyJPEGFixture(f, legacyJPEGSpec{width: 4, height: 4, blockWidth: 4, blockHeight: 4, compression: cJPEG, blocks: [][]byte{j4}})
	seeds = append(seeds, fuzzPatchIFDValue(strip, tStripOffsets, 1<<30))
	seeds = append(seeds, legacyJPEGFixture(f, legacyJPEGSpec{width: 4, height: 4, blockWidth: 4, blockHeight: 4, compression: cJPEG, blocks: [][]byte{j4}, tables: true}))

	j8 := legacyTestJPEG(f, 8, 8)
	tile := legacyJPEGFixture(f, legacyJPEGSpec{width: 9, height: 9, blockWidth: 8, blockHeight: 8, compression: cJPEG, tiled: true, blocks: [][]byte{j8, j8, j8, j8}})
	seeds = append(seeds, tile, fuzzPatchIFDValue(tile, tTileOffsets, 1<<30))
	seeds = append(seeds, legacyJPEGFixture(f, legacyJPEGSpec{width: 4, height: 4, blockWidth: 4, blockHeight: 4, compression: 65000, blocks: [][]byte{j4}}))
	ycbcr, _ := jpegTestTIFF(f, jpegTestSpec{w: 4, h: 4, bw: 4, bh: 4})
	seeds = append(seeds, ycbcr)
	ycbcrTables, _ := jpegTestTIFF(f, jpegTestSpec{w: 4, h: 4, bw: 4, bh: 4, shared: true})
	seeds = append(seeds, ycbcrTables)
	cmyk := fuzzFixture(f, "legacy-cmyk.jpg")
	seeds = append(seeds, legacyJPEGFixture(f, legacyJPEGSpec{width: 150, height: 103, blockWidth: 150, blockHeight: 103, compression: cJPEGOld, blocks: [][]byte{cmyk}, alpha: true}))
	multi, second := fuzzMultiPage(f)

	seeds = append(seeds, multi)
	for _, b := range seeds {
		if len(b) > fuzzMaxInput {
			f.Fatalf("seed of %d bytes exceeds fuzz input limit", len(b))
		}
	}

	return seeds, second
}

func fuzzReaders(b []byte) [2]io.Reader {
	return [2]io.Reader{bytes.NewReader(b), readOnly{bytes.NewReader(b)}}
}

func fuzzConfigOK(c image.Config) bool {
	return c.Width > 0 && c.Height > 0 && c.Width <= fuzzMaxDimension &&
		c.Height <= fuzzMaxDimension && c.Width <= fuzzMaxPixels/c.Height
}

func fuzzOffsetInput(b []byte) ([]byte, int64) {
	if len(b) < 8 {
		return b, 0
	}
	off := int64(binary.LittleEndian.Uint64(b[:8]))
	payload := b[8:]
	if off > fuzzMaxOffset || off < -fuzzMaxOffset {
		off %= int64(len(payload) + 1)
	}
	return payload, off
}

func fuzzAtSeed(b []byte, off int64) []byte {
	seed := make([]byte, 8, 8+len(b))
	binary.LittleEndian.PutUint64(seed, uint64(off))
	return append(seed, b...)
}

// FuzzDecodeConfig exercises TIFF configuration parsing with both reader paths.
func FuzzDecodeConfig(f *testing.F) {
	seeds, _ := fuzzTIFFSeeds(f)
	for _, b := range seeds {
		f.Add(b)
	}
	f.Fuzz(func(_ *testing.T, b []byte) {
		if len(b) > fuzzMaxInput {
			return
		}
		for _, r := range fuzzReaders(b) {
			_, _ = DecodeConfig(r)
		}
	})
}

// FuzzDecode exercises full TIFF decoding after a bounded configuration check.
func FuzzDecode(f *testing.F) {
	seeds, _ := fuzzTIFFSeeds(f)
	for _, b := range seeds {
		f.Add(b)
	}
	f.Fuzz(func(_ *testing.T, b []byte) {
		if len(b) > fuzzMaxInput {
			return
		}
		for _, r := range fuzzReaders(b) {
			c, err := DecodeConfig(r)
			if err == nil && fuzzConfigOK(c) {
				_, _ = Decode(fuzzFreshReader(r, b))
			}
		}
	})
}

func fuzzFreshReader(r io.Reader, b []byte) io.Reader {
	if _, ok := r.(io.ReaderAt); ok {
		return bytes.NewReader(b)
	}
	return readOnly{bytes.NewReader(b)}
}

// FuzzDecodeConfigAt exercises explicit IFD offsets with both reader paths.
func FuzzDecodeConfigAt(f *testing.F) {
	seeds, second := fuzzTIFFSeeds(f)
	for _, b := range seeds {
		f.Add(fuzzAtSeed(b, 0))
	}
	multi := seeds[len(seeds)-1]
	f.Add(fuzzAtSeed(multi, second))
	f.Add(fuzzAtSeed(multi, -1))
	f.Add(fuzzAtSeed(multi, int64(len(multi)+1)))
	f.Add(fuzzAtSeed(multi, 1<<32))
	f.Add(fuzzAtSeed(multi, fuzzMaxOffset))
	f.Fuzz(func(_ *testing.T, b []byte) {
		if len(b) > fuzzMaxInput {
			return
		}
		payload, off := fuzzOffsetInput(b)
		for _, r := range fuzzReaders(payload) {
			_, _ = DecodeConfigAt(r, off)
		}
	})
}

// FuzzDecodeAt exercises decoding at explicit IFD offsets after a bounded configuration check.
func FuzzDecodeAt(f *testing.F) {
	seeds, second := fuzzTIFFSeeds(f)
	for _, b := range seeds {
		f.Add(fuzzAtSeed(b, 0))
	}
	multi := seeds[len(seeds)-1]
	f.Add(fuzzAtSeed(multi, second))
	f.Add(fuzzAtSeed(multi, -1))
	f.Add(fuzzAtSeed(multi, int64(len(multi)+1)))
	f.Add(fuzzAtSeed(multi, 1<<32))
	f.Add(fuzzAtSeed(multi, fuzzMaxOffset))
	f.Fuzz(func(_ *testing.T, b []byte) {
		if len(b) > fuzzMaxInput {
			return
		}
		payload, off := fuzzOffsetInput(b)
		for _, r := range fuzzReaders(payload) {
			c, err := DecodeConfigAt(r, off)
			if err == nil && fuzzConfigOK(c) {
				_, _ = DecodeAt(fuzzFreshReader(r, payload), off)
			}
		}
	})
}
