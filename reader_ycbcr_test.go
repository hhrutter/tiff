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
	"image/draw"
	"image/jpeg"
	"io"
	"os"
	"sort"
	"testing"
)

type jpegTestField struct {
	kind  uint16
	count uint32
	data  []byte
}

type jpegTestSpec struct {
	w, h, bw, bh  int
	tiled, shared bool
	order         binary.ByteOrder
	sampling      [2]uint16
	payload       []byte
	base          uint32
	edit          func(map[int]jpegTestField)
	editJPEG      func([]byte) []byte
}

func jpegTestShort(order binary.ByteOrder, values ...uint16) jpegTestField {
	b := make([]byte, 2*len(values))
	for i, v := range values {
		order.PutUint16(b[2*i:], v)
	}
	return jpegTestField{dtShort, uint32(len(values)), b}
}

func jpegTestLong(order binary.ByteOrder, values ...uint32) jpegTestField {
	b := make([]byte, 4*len(values))
	for i, v := range values {
		order.PutUint32(b[4*i:], v)
	}
	return jpegTestField{dtLong, uint32(len(values)), b}
}

func jpegTestRational(order binary.ByteOrder, values ...uint32) jpegTestField {
	f := jpegTestLong(order, values...)
	f.kind, f.count = dtRational, uint32(len(values)/2)
	return f
}

func encodeTestJPEG(t *testing.T, w, h, index int) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			im.SetRGBA(x, y, color.RGBA{uint8(x*13 + index*39), uint8(y*11 + index*23), uint8(x*3 + y*7), 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, im, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// splitTestJPEG operates on known encoder output, independently of the TIFF JPEG marker reader.
func splitTestJPEG(b []byte) ([]byte, []byte) {
	tables, segment := []byte{0xff, 0xd8}, []byte{0xff, 0xd8}
	for pos := 2; pos < len(b); {
		marker := b[pos+1]
		if marker == 0xda {
			segment = append(segment, b[pos:]...)
			break
		}
		end := pos + 2 + int(binary.BigEndian.Uint16(b[pos+2:]))
		if marker == 0xdb || marker == 0xc4 {
			tables = append(tables, b[pos:end]...)
		} else {
			segment = append(segment, b[pos:end]...)
		}
		pos = end
	}
	return append(tables, 0xff, 0xd9), segment
}

func jpegTestSegments(t *testing.T, s jpegTestSpec) ([][]byte, *image.RGBA) {
	t.Helper()
	want := image.NewRGBA(image.Rect(0, 0, s.w, s.h))
	var parts [][]byte
	for y := 0; y < s.h; y += s.bh {
		for x := 0; x < s.w; x += s.bw {
			h := s.bh
			if !s.tiled && y+h > s.h {
				h = s.h - y
			}
			b := s.payload
			if b == nil {
				b = encodeTestJPEG(t, s.bw, h, len(parts))
			}
			im, err := jpeg.Decode(bytes.NewReader(b))
			if err != nil {
				t.Fatal(err)
			}
			draw.Draw(want, image.Rect(x, y, x+s.bw, y+h), im, image.Point{}, draw.Src)
			if s.editJPEG != nil {
				b = s.editJPEG(append([]byte(nil), b...))
			}
			parts = append(parts, b)
		}
	}
	return parts, want
}

func jpegTestFields(s jpegTestSpec) map[int]jpegTestField {
	o := s.order
	return map[int]jpegTestField{
		tImageWidth: jpegTestLong(o, uint32(s.w)), tImageLength: jpegTestLong(o, uint32(s.h)),
		tBitsPerSample: jpegTestShort(o, 8, 8, 8), tCompression: jpegTestShort(o, cJPEG),
		tPhotometricInterpretation: jpegTestShort(o, pYCbCr), tSamplesPerPixel: jpegTestShort(o, 3),
		tPlanarConfiguration: jpegTestShort(o, 1), tYCbCrPositioning: jpegTestShort(o, 1),
		tYCbCrSubSampling:    jpegTestShort(o, s.sampling[0], s.sampling[1]),
		tReferenceBlackWhite: jpegTestRational(o, 0, 1, 255, 1, 128, 1, 255, 1, 128, 1, 255, 1),
	}
}

func jpegTestTIFF(t *testing.T, s jpegTestSpec) ([]byte, *image.RGBA) {
	t.Helper()
	if s.order == nil {
		s.order = binary.LittleEndian
	}
	if s.sampling == [2]uint16{} {
		s.sampling = [2]uint16{2, 2}
	}
	parts, want := jpegTestSegments(t, s)
	fields := jpegTestFields(s)
	var counts, offsets []uint32
	var tables, pixels []byte
	for _, original := range parts {
		b := original
		if s.shared {
			tables, b = splitTestJPEG(b)
		}
		offsets = append(offsets, s.base+uint32(8+len(pixels)))
		counts = append(counts, uint32(len(b)))
		pixels = append(pixels, b...)
	}
	if s.shared {
		fields[tJPEGTables] = jpegTestField{7, uint32(len(tables)), tables}
	}
	if s.tiled {
		fields[tTileWidth], fields[tTileLength] = jpegTestLong(s.order, uint32(s.bw)), jpegTestLong(s.order, uint32(s.bh))
		fields[tTileOffsets], fields[tTileByteCounts] = jpegTestLong(s.order, offsets...), jpegTestLong(s.order, counts...)
	} else {
		fields[tRowsPerStrip] = jpegTestLong(s.order, uint32(s.bh))
		fields[tStripOffsets], fields[tStripByteCounts] = jpegTestLong(s.order, offsets...), jpegTestLong(s.order, counts...)
	}
	if s.edit != nil {
		s.edit(fields)
	}
	return packJPEGTestTIFF(s, pixels, fields), want
}

func packJPEGTestTIFF(s jpegTestSpec, pixels []byte, fields map[int]jpegTestField) []byte {
	tags := make([]int, 0, len(fields))
	for tag := range fields {
		tags = append(tags, tag)
	}
	sort.Ints(tags)
	b := []byte(leHeader + "\x00\x00\x00\x00")
	if s.order == binary.BigEndian {
		copy(b, beHeader)
	}
	b = append(b, pixels...)
	if len(b)%2 != 0 {
		b = append(b, 0)
	}
	off := len(b)
	s.order.PutUint32(b[4:], s.base+uint32(off))
	b = append(b, make([]byte, 2+12*len(tags)+4)...)
	s.order.PutUint16(b[off:], uint16(len(tags)))
	for i, tag := range tags {
		f, pos := fields[tag], off+2+12*i
		s.order.PutUint16(b[pos:], uint16(tag))
		s.order.PutUint16(b[pos+2:], f.kind)
		s.order.PutUint32(b[pos+4:], f.count)
		if len(f.data) <= 4 {
			copy(b[pos+8:pos+12], f.data)
			continue
		}
		s.order.PutUint32(b[pos+8:], s.base+uint32(len(b)))
		b = append(b, f.data...)
		if len(b)%2 != 0 {
			b = append(b, 0)
		}
	}
	return b
}

func checkJPEGTestImage(t *testing.T, got image.Image, err error, want *image.RGBA) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	rgba, ok := got.(*image.RGBA)
	if !ok {
		t.Fatalf("image type = %T, want *image.RGBA", got)
	}
	if rgba.Bounds() != want.Bounds() || !bytes.Equal(rgba.Pix, want.Pix) {
		t.Fatal("decoded TIFF dimensions or pixels differ from direct JPEG decoding")
	}
}

func checkJPEGTestConfig(t *testing.T, b []byte, offset int64, want *image.RGBA) {
	t.Helper()
	c, err := DecodeConfigAt(bytes.NewReader(b), offset)
	if err != nil {
		t.Fatal(err)
	}
	if c.Width != want.Bounds().Dx() || c.Height != want.Bounds().Dy() || c.ColorModel != color.RGBAModel {
		t.Fatalf("unexpected config: %+v", c)
	}
}

func testDecodeYCbCrJPEG(t *testing.T) {
	cases := []struct {
		name string
		spec jpegTestSpec
	}{
		{"single", jpegTestSpec{w: 17, h: 19, bw: 17, bh: 19}},
		{"strips", jpegTestSpec{w: 17, h: 19, bw: 17, bh: 16}},
		{"strips_tables", jpegTestSpec{w: 17, h: 19, bw: 17, bh: 16, shared: true}},
		{"tiles", jpegTestSpec{w: 17, h: 19, bw: 16, bh: 16, tiled: true}},
		{"tiles_tables", jpegTestSpec{w: 17, h: 19, bw: 16, bh: 16, tiled: true, shared: true}},
		{"tiny", jpegTestSpec{w: 1, h: 1, bw: 1, bh: 1}},
		{"tiny_tables", jpegTestSpec{w: 1, h: 1, bw: 1, bh: 1, shared: true}},
	}
	for _, tc := range cases {
		for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
			t.Run(tc.name+"/"+order.String(), func(t *testing.T) {
				s := tc.spec
				s.order = order
				b, want := jpegTestTIFF(t, s)
				checkJPEGTestConfig(t, b, 0, want)
				got, err := Decode(bytes.NewReader(b))
				checkJPEGTestImage(t, got, err, want)
				got, err = Decode(struct{ io.Reader }{bytes.NewReader(b)})
				checkJPEGTestImage(t, got, err, want)
			})
		}
	}
}

// TestYCbCrJPEGSubsampling covers independently encoded 4:4:4 and 4:2:2 JPEG fixtures.
func TestYCbCrJPEGSubsampling(t *testing.T) {
	for _, tc := range []struct {
		name    string
		factors [2]uint16
	}{{"444", [2]uint16{1, 1}}, {"422", [2]uint16{2, 1}}} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := os.ReadFile("testdata/ycbcr-" + tc.name + ".jpg")
			if err != nil {
				t.Fatal(err)
			}
			for _, shared := range []bool{false, true} {
				s := jpegTestSpec{w: 16, h: 16, bw: 16, bh: 16, payload: payload, sampling: tc.factors, shared: shared}
				b, want := jpegTestTIFF(t, s)
				got, err := Decode(bytes.NewReader(b))
				checkJPEGTestImage(t, got, err, want)
			}
		})
	}
}

// TestYCbCrJPEGDefaults checks permitted omissions and equivalent rational representations.
func TestYCbCrJPEGDefaults(t *testing.T) {
	s := jpegTestSpec{w: 16, h: 16, bw: 16, bh: 16}
	s.edit = func(f map[int]jpegTestField) {
		delete(f, tPlanarConfiguration)
		delete(f, tYCbCrSubSampling)
		delete(f, tYCbCrPositioning)
		delete(f, tRowsPerStrip)
		f[tYCbCrCoefficients] = jpegTestRational(binary.LittleEndian, 598, 2000, 1174, 2000, 228, 2000)
		f[tReferenceBlackWhite] = jpegTestRational(binary.LittleEndian, 0, 2, 510, 2, 256, 2, 510, 2, 256, 2, 510, 2)
	}
	b, want := jpegTestTIFF(t, s)
	got, err := Decode(bytes.NewReader(b))
	checkJPEGTestImage(t, got, err, want)
}

// TestYCbCrJPEGPages checks both explicit IFD entry points against a second image in the same file.
func TestYCbCrJPEGPages(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		s := jpegTestSpec{w: 17, h: 19, bw: 17, bh: 16, order: order, shared: true}
		first, wantFirst := jpegTestTIFF(t, s)
		s.w, s.h, s.bw, s.bh, s.base = 16, 16, 16, 16, uint32(len(first))
		second, wantSecond := jpegTestTIFF(t, s)
		firstIFD := order.Uint32(first[4:])
		secondIFD := order.Uint32(second[4:])
		n := order.Uint16(first[firstIFD:])
		order.PutUint32(first[int(firstIFD)+2+12*int(n):], secondIFD)
		b := append(first, second...)
		got, err := Decode(bytes.NewReader(b))
		checkJPEGTestImage(t, got, err, wantFirst)
		checkJPEGTestConfig(t, b, int64(secondIFD), wantSecond)
		got, err = DecodeAt(bytes.NewReader(b), int64(secondIFD))
		checkJPEGTestImage(t, got, err, wantSecond)
	}
}

func checkYCbCrError(t *testing.T, b []byte, unsupported bool) {
	t.Helper()
	_, err := Decode(bytes.NewReader(b))
	if err == nil {
		t.Fatal("Decode accepted invalid or unsupported YCbCr metadata")
	}
	var u UnsupportedError
	var f FormatError
	if unsupported && !errors.As(err, &u) {
		t.Fatalf("error = %T: %v, want UnsupportedError", err, err)
	}
	if !unsupported && !errors.As(err, &f) {
		t.Fatalf("error = %T: %v, want FormatError", err, err)
	}
	if _, configErr := DecodeConfig(bytes.NewReader(b)); configErr == nil {
		t.Fatal("DecodeConfig accepted invalid or unsupported YCbCr metadata")
	}
}

// TestYCbCrJPEGMetadata rejects unsupported features and malformed field shapes before JPEG decoding.
func TestYCbCrJPEGMetadata(t *testing.T) {
	o := binary.LittleEndian
	cases := []struct {
		name        string
		tag         int
		field       jpegTestField
		unsupported bool
	}{
		{"uncompressed", tCompression, jpegTestShort(o, cNone), true},
		{"lzw", tCompression, jpegTestShort(o, cLZW), true},
		{"old_jpeg", tCompression, jpegTestShort(o, cJPEGOld), true},
		{"deflate", tCompression, jpegTestShort(o, cDeflate), true},
		{"packbits", tCompression, jpegTestShort(o, cPackBits), true},
		{"planar", tPlanarConfiguration, jpegTestShort(o, 2), true},
		{"invalid_planar", tPlanarConfiguration, jpegTestShort(o, 3), false},
		{"extra_component", tSamplesPerPixel, jpegTestShort(o, 4), true},
		{"zero_samples", tSamplesPerPixel, jpegTestShort(o, 0), false},
		{"extra_samples", tExtraSamples, jpegTestShort(o, 1), true},
		{"sixteen_bits", tBitsPerSample, jpegTestShort(o, 16, 16, 16), true},
		{"mixed_bits", tBitsPerSample, jpegTestShort(o, 8, 8, 16), true},
		{"zero_bits", tBitsPerSample, jpegTestShort(o, 0, 8, 8), false},
		{"zero_rows", tRowsPerStrip, jpegTestLong(o, 0), false},
		{"zero_width", tImageWidth, jpegTestLong(o, 0), false},
		{"width_count", tImageWidth, jpegTestLong(o, 16, 16), false},
		{"width_type", tImageWidth, jpegTestField{dtByte, 1, []byte{16}}, false},
		{"incomplete_tiles", tTileLength, jpegTestLong(o, 16), false},
		{"bits_count", tBitsPerSample, jpegTestShort(o, 8), false},
		{"samples_type", tSamplesPerPixel, jpegTestLong(o, 3), false},
		{"subsampling_type", tYCbCrSubSampling, jpegTestLong(o, 2, 2), false},
		{"subsampling_count", tYCbCrSubSampling, jpegTestShort(o, 2), false},
		{"subsampling_zero", tYCbCrSubSampling, jpegTestShort(o, 0, 0), false},
		{"subsampling_order", tYCbCrSubSampling, jpegTestShort(o, 1, 2), false},
		{"subsampling_four", tYCbCrSubSampling, jpegTestShort(o, 4, 1), true},
		{"cosited", tYCbCrPositioning, jpegTestShort(o, 2), true},
		{"invalid_position", tYCbCrPositioning, jpegTestShort(o, 3), false},
		{"predictor", tPredictor, jpegTestShort(o, 2), false},
		{"coefficients", tYCbCrCoefficients, jpegTestRational(o, 2125, 10000, 7154, 10000, 721, 10000), true},
		{"coefficients_type", tYCbCrCoefficients, jpegTestShort(o, 1, 1, 1), false},
		{"coefficients_count", tYCbCrCoefficients, jpegTestRational(o, 299, 1000), false},
		{"zero_denominator", tYCbCrCoefficients, jpegTestRational(o, 299, 0, 587, 1000, 114, 1000), false},
		{"reference_range", tReferenceBlackWhite, jpegTestRational(o, 16, 1, 235, 1, 128, 1, 240, 1, 128, 1, 240, 1), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := jpegTestSpec{w: 16, h: 16, bw: 16, bh: 16}
			s.edit = func(f map[int]jpegTestField) { f[tc.tag] = tc.field }
			b, _ := jpegTestTIFF(t, s)
			checkYCbCrError(t, b, tc.unsupported)
		})
	}
	for _, tag := range []int{tImageWidth, tImageLength, tCompression, tSamplesPerPixel, tBitsPerSample, tReferenceBlackWhite} {
		t.Run(fmt.Sprintf("missing_%d", tag), func(t *testing.T) {
			s := jpegTestSpec{w: 16, h: 16, bw: 16, bh: 16}
			s.edit = func(f map[int]jpegTestField) { delete(f, tag) }
			b, _ := jpegTestTIFF(t, s)
			checkYCbCrError(t, b, false)
		})
	}
}

// TestYCbCrJPEGOffsetFields rejects malformed segment offset and byte-count arrays.
func TestYCbCrJPEGOffsetFields(t *testing.T) {
	for _, tiled := range []bool{false, true} {
		tags := []int{tStripOffsets, tStripByteCounts}
		if tiled {
			tags = []int{tTileOffsets, tTileByteCounts}
		}
		for _, tag := range tags {
			for _, kind := range []string{"missing", "empty", "extra", "byte"} {
				t.Run(fmt.Sprintf("%d_%s", tag, kind), func(t *testing.T) {
					s := jpegTestSpec{w: 16, h: 16, bw: 16, bh: 16, tiled: tiled}
					s.edit = func(f map[int]jpegTestField) {
						switch kind {
						case "missing":
							delete(f, tag)
						case "empty":
							f[tag] = jpegTestLong(binary.LittleEndian)
						case "extra":
							f[tag] = jpegTestLong(binary.LittleEndian, 8, 8)
						case "byte":
							f[tag] = jpegTestField{dtByte, 1, []byte{8}}
						}
					}
					b, _ := jpegTestTIFF(t, s)
					_, err := Decode(bytes.NewReader(b))
					var formatErr FormatError
					if !errors.As(err, &formatErr) {
						t.Fatalf("Decode error = %v, want FormatError", err)
					}
				})
			}
		}
	}
}
