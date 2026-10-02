// Copyright 2026 The tiff Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tiff

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

func readJPEGStart(r io.Reader) error {
	var b [2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return err
	}
	if b != [2]byte{0xff, 0xd8} {
		return FormatError("missing JPEG SOI marker")
	}
	return nil
}

// readJPEGMarker reads a header marker, respecting segment lengths rather than searching payload bytes.
func readJPEGMarker(r io.Reader) (byte, []byte, error) {
	var b [2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, nil, err
	}
	if b[0] != 0xff {
		return 0, nil, FormatError("invalid JPEG marker prefix")
	}
	for b[1] == 0xff {
		if _, err := io.ReadFull(r, b[1:]); err != nil {
			return 0, nil, err
		}
	}
	marker := b[1]
	if marker == 0xd8 || marker == 0xd9 {
		return marker, nil, nil
	}
	if marker == 0 || marker == 1 || (marker >= 0xd0 && marker <= 0xd7) {
		return 0, nil, FormatError("unexpected standalone JPEG marker")
	}
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint16(b[:]))
	if n < 2 {
		return 0, nil, FormatError("invalid JPEG marker length")
	}
	p := make([]byte, n-2)
	_, err := io.ReadFull(r, p)
	return marker, p, err
}

func appendJPEGMarker(dst []byte, marker byte, p []byte) []byte {
	n := len(p) + 2
	dst = append(dst, 0xff, marker, byte(n>>8), byte(n))
	return append(dst, p...)
}

func jpegIgnoredMarker(marker byte) bool {
	// See TIFF Technical Note #2: APPn and COM must not determine TIFF color interpretation.
	return (marker >= 0xe0 && marker <= 0xef) || marker == 0xfe
}

func (d *decoder) jpegTables() ([]byte, error) {
	p, found := d.ifd[tJPEGTables]
	if !found {
		return nil, nil
	}
	n := d.byteOrder.Uint32(p[4:8])
	if d.byteOrder.Uint16(p[2:4]) != 7 || n < 4 {
		return nil, FormatError("invalid JPEGTables type or count")
	}
	var r io.Reader = bytes.NewReader(p[8:12])
	if n > 4 {
		r = io.NewSectionReader(d.r, int64(d.byteOrder.Uint32(p[8:12])), int64(n))
	}
	if err := readJPEGStart(r); err != nil {
		return nil, err
	}
	return readJPEGTables(r)
}

type jpegTableSet struct {
	quant   [4][]byte
	huffman [2][4][]byte
}

func (s *jpegTableSet) add(marker byte, p []byte) error {
	switch marker {
	case 0xdb:
		return s.addQuantization(p)
	case 0xc4:
		return s.addHuffman(p)
	}
	return UnsupportedError(fmt.Sprintf("JPEG table marker 0x%02x", marker))
}

func (s *jpegTableSet) addQuantization(p []byte) error {
	for len(p) > 0 {
		precision, index := p[0]>>4, p[0]&15
		if precision > 1 || index >= byte(len(s.quant)) {
			return FormatError("invalid JPEG quantization table")
		}
		n := 65 + 64*int(precision)
		if len(p) < n {
			return FormatError("invalid JPEG quantization table length")
		}
		s.quant[index] = appendJPEGMarker(nil, 0xdb, p[:n])
		p = p[n:]
	}
	return nil
}

func (s *jpegTableSet) addHuffman(p []byte) error {
	for len(p) > 0 {
		if len(p) < 17 {
			return FormatError("invalid JPEG Huffman table length")
		}
		class, index := p[0]>>4, p[0]&15
		if class >= byte(len(s.huffman)) || index >= byte(len(s.huffman[0])) {
			return FormatError("invalid JPEG Huffman table")
		}
		n := 17
		for _, count := range p[1:17] {
			n += int(count)
		}
		if n > len(p) || n > 17+256 {
			return FormatError("invalid JPEG Huffman table length")
		}
		s.huffman[class][index] = appendJPEGMarker(nil, 0xc4, p[:n])
		p = p[n:]
	}
	return nil
}

func (s *jpegTableSet) bytes() []byte {
	var b []byte
	for _, table := range s.quant {
		b = append(b, table...)
	}
	for _, class := range s.huffman {
		for _, table := range class {
			b = append(b, table...)
		}
	}
	return b
}

func readJPEGTables(r io.Reader) ([]byte, error) {
	var tables jpegTableSet
	for {
		marker, p, err := readJPEGMarker(r)
		if err != nil {
			return nil, err
		}
		switch {
		case marker == 0xd9:
			var b [1]byte
			if n, err := r.Read(b[:]); n != 0 || err != io.EOF {
				return nil, FormatError("data after JPEGTables EOI")
			}
			return tables.bytes(), nil
		case marker == 0xdb || marker == 0xc4:
			if err := tables.add(marker, p); err != nil {
				return nil, err
			}
		case jpegIgnoredMarker(marker), marker == 0xdd:
			// DRI state is reset by the image segment's SOI; see TIFF Technical Note #2.
		default:
			return nil, UnsupportedError(fmt.Sprintf("JPEGTables marker 0x%02x", marker))
		}
	}
}

type ycbcrJPEGHeader struct {
	tables  jpegTableSet
	frame   []byte
	restart []byte
	ids     [3]byte
}

func (d *decoder) ycbcrJPEGReader(offset, n int64, width, height int, tables []byte) (io.Reader, error) {
	r := io.NewSectionReader(d.r, offset, n)
	if err := readJPEGStart(r); err != nil {
		return nil, err
	}
	var h ycbcrJPEGHeader
	for {
		marker, p, err := readJPEGMarker(r)
		if err != nil {
			return nil, err
		}
		if marker == 0xda {
			if err := h.scan(p); err != nil {
				return nil, err
			}
			prefix := append([]byte{0xff, 0xd8}, tables...)
			prefix = append(prefix, h.tables.bytes()...)
			prefix = append(prefix, h.frame...)
			prefix = append(prefix, h.restart...)
			prefix = appendJPEGMarker(prefix, marker, p)
			scan := &ycbcrJPEGScan{r: bufio.NewReader(r)}
			return io.MultiReader(bytes.NewReader(prefix), scan), nil
		}
		if err := d.ycbcrJPEGHeaderMarker(&h, marker, p, width, height); err != nil {
			return nil, err
		}
	}
}

func (d *decoder) ycbcrJPEGHeaderMarker(h *ycbcrJPEGHeader, marker byte, p []byte, width, height int) error {
	switch {
	case marker == 0xc0 || marker == 0xc1:
		if h.frame != nil {
			return FormatError("multiple JPEG frames")
		}
		if err := d.ycbcrJPEGFrame(h, p, width, height); err != nil {
			return err
		}
		h.frame = appendJPEGMarker(nil, marker, p)
	case marker == 0xdb || marker == 0xc4:
		return h.tables.add(marker, p)
	case marker == 0xdd:
		if len(p) != 2 {
			return FormatError("invalid JPEG restart interval")
		}
		h.restart = appendJPEGMarker(nil, marker, p)
	case jpegIgnoredMarker(marker):
		return nil
	case marker == 0xd8 || marker == 0xd9:
		return FormatError("unexpected JPEG image boundary")
	default:
		return UnsupportedError(fmt.Sprintf("YCbCr JPEG marker 0x%02x", marker))
	}
	return nil
}

func (d *decoder) ycbcrJPEGFrame(h *ycbcrJPEGHeader, p []byte, width, height int) error {
	if len(p) != 15 || p[5] != 3 {
		return FormatError("JPEG component count does not match YCbCr TIFF")
	}
	if p[0] != 8 {
		return FormatError("JPEG precision does not match YCbCr TIFF")
	}
	if int(binary.BigEndian.Uint16(p[1:3])) != height || int(binary.BigEndian.Uint16(p[3:5])) != width {
		return FormatError("JPEG dimensions do not match TIFF segment")
	}
	if err := d.checkJPEGSampling(p); err != nil {
		return err
	}
	for i := range h.ids {
		h.ids[i] = p[6+3*i]
		// JPEG component IDs are arbitrary in TIFF; neutral IDs prevent Go's RGB heuristic.
		p[6+3*i] = byte(i + 1)
	}
	if h.ids[0] == h.ids[1] || h.ids[0] == h.ids[2] || h.ids[1] == h.ids[2] {
		return FormatError("duplicate JPEG component ID")
	}
	return nil
}

func (d *decoder) checkJPEGSampling(p []byte) error {
	for _, sampling := range []byte{p[7], p[10], p[13]} {
		h, v := sampling>>4, sampling&15
		if h == 0 || h > 4 || v == 0 || v > 4 {
			return FormatError("invalid JPEG sampling factors")
		}
	}
	yh, yv := uint16(p[7]>>4), uint16(p[7]&15)
	ch, cv := uint16(p[10]>>4), uint16(p[10]&15)
	if p[10] != p[13] || yh != ch*d.ycbcr.horizontal || yv != cv*d.ycbcr.vertical {
		return FormatError("JPEG sampling does not match YCbCrSubSampling")
	}
	return nil
}

func (h *ycbcrJPEGHeader) scan(p []byte) error {
	if h.frame == nil {
		return FormatError("JPEG scan before frame")
	}
	if len(p) != 10 || p[0] != 3 {
		return UnsupportedError("noninterleaved YCbCr JPEG scan")
	}
	for i, id := range h.ids {
		if p[1+2*i] != id {
			return FormatError("JPEG scan component order does not match TIFF")
		}
		p[1+2*i] = byte(i + 1)
	}
	if p[7] != 0 || p[8] != 63 || p[9] != 0 {
		return UnsupportedError("nonsequential YCbCr JPEG scan")
	}
	return nil
}

// ycbcrJPEGScan filters metadata after SOS as well, so trailing APP markers cannot change the color model.
type ycbcrJPEGScan struct {
	r       *bufio.Reader
	pending [2]byte
	start   int
	end     int
	done    bool
}

// Read forwards entropy data, restart markers, and EOI for a single interleaved sequential scan.
func (s *ycbcrJPEGScan) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if s.start < s.end {
			copied := copy(p[n:], s.pending[s.start:s.end])
			s.start += copied
			n += copied
			continue
		}
		if s.done {
			return n, io.EOF
		}
		s.start = 0
		var err error
		s.end, err = s.next()
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (s *ycbcrJPEGScan) next() (int, error) {
	c, err := s.r.ReadByte()
	if err != nil {
		return 0, err
	}
	s.pending[0] = c
	if c != 0xff {
		return 1, nil
	}
	for c == 0xff {
		c, err = s.r.ReadByte()
		if err != nil {
			return 0, err
		}
	}
	s.pending[1] = c
	switch {
	case c == 0 || (c >= 0xd0 && c <= 0xd7):
		return 2, nil
	case c == 0xd9:
		s.done = true
		return 2, nil
	case jpegIgnoredMarker(c):
		return 0, skipJPEGMarker(s.r)
	default:
		return 0, UnsupportedError(fmt.Sprintf("YCbCr JPEG scan marker 0x%02x", c))
	}
}

func skipJPEGMarker(r io.Reader) error {
	var b [2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return err
	}
	n := int64(binary.BigEndian.Uint16(b[:]))
	if n < 2 {
		return FormatError("invalid JPEG marker length")
	}
	_, err := io.CopyN(io.Discard, r, n-2)
	return err
}
