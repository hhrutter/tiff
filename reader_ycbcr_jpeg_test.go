// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tiff

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func jpegTestMarker(t *testing.T, b []byte, marker byte) int {
	t.Helper()
	for pos := 2; pos < len(b); {
		if b[pos+1] == marker {
			return pos
		}
		if b[pos+1] == 0xda {
			break
		}
		pos += 2 + int(binary.BigEndian.Uint16(b[pos+2:]))
	}
	t.Fatalf("test JPEG has no marker %02x", marker)
	return 0
}

func insertTestJPEGMarker(b []byte, marker byte, payload []byte, trailing bool) []byte {
	pos := 2
	if trailing {
		pos = len(b) - 2
	}
	n := len(payload) + 2
	result := append([]byte(nil), b[:pos]...)
	result = append(result, 0xff, marker, byte(n>>8), byte(n))
	result = append(result, payload...)
	return append(result, b[pos:]...)
}

// TestYCbCrJPEGColorInterpretation ensures TIFF color semantics prevail over JPEG metadata and component IDs.
func TestYCbCrJPEGColorInterpretation(t *testing.T) {
	adobeRGB := []byte{'A', 'd', 'o', 'b', 'e', 0, 100, 0, 0, 0, 0, 0}
	cases := []struct {
		name string
		edit func([]byte) []byte
	}{
		{"adobe_rgb", func(b []byte) []byte { return insertTestJPEGMarker(b, 0xee, adobeRGB, false) }},
		{"trailing_adobe_rgb", func(b []byte) []byte { return insertTestJPEGMarker(b, 0xee, adobeRGB, true) }},
		{"comment_with_sos", func(b []byte) []byte {
			return insertTestJPEGMarker(b, 0xfe, []byte{'a', 0xff, 0xda, 'b'}, false)
		}},
		{"rgb_component_ids", func(b []byte) []byte {
			frame, scan := jpegTestMarker(t, b, 0xc0)+4, jpegTestMarker(t, b, 0xda)+4
			for i, id := range []byte{'R', 'G', 'B'} {
				b[frame+6+3*i], b[scan+1+2*i] = id, id
			}
			return b
		}},
	}
	for _, tc := range cases {
		for _, shared := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/shared=%v", tc.name, shared), func(t *testing.T) {
				s := jpegTestSpec{w: 16, h: 16, bw: 16, bh: 16, shared: shared, editJPEG: tc.edit}
				b, want := jpegTestTIFF(t, s)
				got, err := Decode(bytes.NewReader(b))
				checkJPEGTestImage(t, got, err, want)
			})
		}
	}
}

// TestYCbCrJPEGInvalidFrames rejects inconsistent headers before JPEG pixel allocation.
func TestYCbCrJPEGInvalidFrames(t *testing.T) {
	cases := []struct {
		name   string
		marker byte
		index  int
		value  byte
	}{
		{"precision", 0xc0, 0, 12},
		{"height", 0xc0, 2, 15},
		{"width", 0xc0, 4, 15},
		{"large_width", 0xc0, 3, 255},
		{"components", 0xc0, 5, 1},
		{"duplicate_id", 0xc0, 9, 1},
		{"zero_sampling", 0xc0, 7, 0},
		{"mismatched_sampling", 0xc0, 7, 0x11},
		{"unequal_chroma", 0xc0, 10, 0x21},
		{"scan_id", 0xda, 1, 42},
		{"noninterleaved", 0xda, 0, 1},
		{"scan_parameters", 0xda, 7, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := jpegTestSpec{w: 16, h: 16, bw: 16, bh: 16}
			s.editJPEG = func(b []byte) []byte {
				pos := jpegTestMarker(t, b, tc.marker) + 4
				b[pos+tc.index] = tc.value
				return b
			}
			b, _ := jpegTestTIFF(t, s)
			if _, err := Decode(bytes.NewReader(b)); err == nil {
				t.Fatal("accepted inconsistent JPEG header")
			}
		})
	}
}

// TestYCbCrJPEGTruncated checks every prefix of a JPEG stream for a returned error rather than a panic.
func TestYCbCrJPEGTruncated(t *testing.T) {
	payload := encodeTestJPEG(t, 16, 16, 0)
	for n := 0; n < len(payload); n++ {
		s := jpegTestSpec{w: 16, h: 16, bw: 16, bh: 16, payload: payload}
		s.editJPEG = func(b []byte) []byte { return b[:n] }
		b, _ := jpegTestTIFF(t, s)
		if _, err := Decode(bytes.NewReader(b)); err == nil {
			t.Fatalf("accepted JPEG truncated to %d of %d bytes", n, len(payload))
		}
	}
}

// TestYCbCrJPEGTables covers empty inline tables, metadata markers, and malformed table streams.
func TestYCbCrJPEGTables(t *testing.T) {
	cases := []struct {
		name  string
		data  []byte
		valid bool
	}{
		{"empty", []byte{0xff, 0xd8, 0xff, 0xd9}, true},
		{"comment", []byte{0xff, 0xd8, 0xff, 0xfe, 0, 4, 0xff, 0xda, 0xff, 0xd9}, true},
		{"bad_start", []byte{0, 0, 0xff, 0xd9}, false},
		{"bad_end", []byte{0xff, 0xd8, 0, 0}, false},
		{"bad_length", []byte{0xff, 0xd8, 0xff, 0xdb, 0, 1, 0xff, 0xd9}, false},
		{"truncated", []byte{0xff, 0xd8, 0xff, 0xdb, 0, 8, 0xff, 0xd9}, false},
		{"bad_quantization_table", []byte{0xff, 0xd8, 0xff, 0xdb, 0, 3, 0x20, 0xff, 0xd9}, false},
		{"bad_huffman_table", []byte{0xff, 0xd8, 0xff, 0xc4, 0, 3, 0, 0xff, 0xd9}, false},
		{"trailing", []byte{0xff, 0xd8, 0xff, 0xd9, 0}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := jpegTestSpec{w: 16, h: 16, bw: 16, bh: 16}
			s.edit = func(f map[int]jpegTestField) { f[tJPEGTables] = jpegTestField{7, uint32(len(tc.data)), tc.data} }
			b, want := jpegTestTIFF(t, s)
			got, err := Decode(struct{ io.Reader }{bytes.NewReader(b)})
			if tc.valid {
				checkJPEGTestImage(t, got, err, want)
			} else if err == nil {
				t.Fatal("accepted malformed JPEGTables")
			}
		})
	}
}

// TestYCbCrJPEGWithoutRegistration verifies standalone callers need not import image/jpeg themselves.
func TestYCbCrJPEGWithoutRegistration(t *testing.T) {
	b, _ := jpegTestTIFF(t, jpegTestSpec{w: 16, h: 16, bw: 16, bh: 16})
	dir := t.TempDir()
	fixture := filepath.Join(dir, "image.tif")
	if err := os.WriteFile(fixture, b, 0600); err != nil {
		t.Fatal(err)
	}
	source := `// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
package main
import (
 "os"
 "github.com/hhrutter/tiff"
)
func main() {
 f, err := os.Open(os.Args[1])
 if err != nil { panic(err) }
 defer f.Close()
 if _, err := tiff.Decode(f); err != nil { panic(err) }
}
`
	filename := filepath.Join(dir, "main.go")
	if err := os.WriteFile(filename, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "run", filename, fixture)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("standalone TIFF consumer: %v\n%s", err, out)
	}
}

// TestJPEGSharedTablesTinyGray checks the compressed-size fix in the existing grayscale JPEG path.
func TestJPEGSharedTablesTinyGray(t *testing.T) {
	gray := image.NewGray(image.Rect(0, 0, 1, 1))
	gray.SetGray(0, 0, color.Gray{Y: 96})
	var payload bytes.Buffer
	if err := jpeg.Encode(&payload, gray, nil); err != nil {
		t.Fatal(err)
	}
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		s := jpegTestSpec{w: 1, h: 1, bw: 1, bh: 1, order: order, payload: payload.Bytes(), shared: true}
		s.edit = func(f map[int]jpegTestField) {
			f[tPhotometricInterpretation] = jpegTestShort(order, pBlackIsZero)
			f[tSamplesPerPixel] = jpegTestShort(order, 1)
			f[tBitsPerSample] = jpegTestShort(order, 8)
			delete(f, tYCbCrSubSampling)
			delete(f, tYCbCrPositioning)
			delete(f, tReferenceBlackWhite)
		}
		b, want := jpegTestTIFF(t, s)
		got, err := Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		compare(t, got, want)
	}
}

// TestPreparedJPEGDimensions checks that the limit applies to decoded dimensions, not compressed overhead.
func TestPreparedJPEGDimensions(t *testing.T) {
	for _, tc := range []struct {
		name          string
		width, height int
		limit         int64
		valid         bool
	}{
		{"one_pixel", 1, 1, 8, true},
		{"one_pixel_too_small", 1, 1, 7, false},
		{"two_pixels", 1, 2, 16, true},
		{"two_pixels_too_small", 1, 2, 8, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := encodeTestJPEG(t, tc.width, tc.height, 0)
			d := &decoder{r: bytes.NewReader(payload)}
			r, err := d.prepJPEG(0, int64(len(payload)), tc.limit, false, false)
			if !tc.valid {
				if err == nil {
					t.Fatal("accepted JPEG exceeding the decoded block limit")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := jpeg.Decode(r()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
