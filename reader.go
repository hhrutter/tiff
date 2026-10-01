// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package tiff is an enhanced version of x/image/tiff.
//
// It adds support for LZW compression and the CMYK color model.
//
// More information: https://github.com/hhrutter/tiff
package tiff

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"math"

	"github.com/hhrutter/tiff/internal/lzw"
	"golang.org/x/image/ccitt"
)

// A FormatError reports that the input is not a valid TIFF image.
type FormatError string

func (e FormatError) Error() string {
	return "tiff: invalid format: " + string(e)
}

// An UnsupportedError reports that the input uses a valid but unimplemented feature.
type UnsupportedError string

func (e UnsupportedError) Error() string {
	return "tiff: unsupported feature: " + string(e)
}

var errNoPixels = FormatError("not enough pixel data")

const maxChunkSize = 10 << 20 // 10M

// safeReadAt reads n bytes without preallocating very large slices before the
// reader has proven that the data exists.
func safeReadAt(r io.ReaderAt, n uint64, off int64) ([]byte, error) {
	if int64(n) < 0 || n != uint64(int(n)) {
		return nil, io.ErrUnexpectedEOF
	}

	if n < maxChunkSize {
		buf := make([]byte, n)
		_, err := r.ReadAt(buf, off)
		if err != nil && (err != io.EOF || n > 0) {
			return nil, err
		}
		return buf, nil
	}

	var buf []byte
	buf1 := make([]byte, maxChunkSize)
	for n > 0 {
		next := min(n, maxChunkSize)
		if _, err := r.ReadAt(buf1[:next], off); err != nil {
			return nil, err
		}
		buf = append(buf, buf1[:next]...)
		n -= next
		off += int64(next)
	}
	return buf, nil
}

var app14Marker = []byte{
	0xFF, 0xEE,
	0x00, 0x0E,
	'A', 'd', 'o', 'b', 'e',
	0x00,
	0x64, 0x00,
	0x00, 0x00,
	0x00, 0x00,
	0x00, // RGB
}

type decoder struct {
	r         io.ReaderAt
	byteOrder binary.ByteOrder
	config    image.Config
	mode      imageMode
	bpp       uint
	features  map[int][]uint
	ifd       map[int][ifdLen]byte
	palette   []color.Color
	tables    []byte
	ycbcr     *ycbcrConfig

	buf   []byte
	off   int    // Current offset in buf.
	v     uint32 // Buffer value for reading with arbitrary bit depths.
	nbits uint   // Remaining number of bits in v.
}

// firstVal returns the first uint of the features entry with the given tag,
// or 0 if the tag does not exist.
func (d *decoder) firstVal(tag int) uint {
	f := d.features[tag]
	if len(f) == 0 {
		return 0
	}
	return f[0]
}

// firstIntVal returns the first int of the features entry with the given tag,
// or 0 if the tag does not exist.
func (d *decoder) firstIntVal(tag int) (int, error) {
	v := d.firstVal(tag)
	if v > uint(maxInt()) {
		return 0, FormatError("IFD value too large")
	}
	return int(v), nil
}

func (d *decoder) setRGBAMode(associated bool) {
	if associated {
		d.mode = mRGBA
		if d.bpp == 16 {
			d.config.ColorModel = color.RGBA64Model
		} else {
			d.config.ColorModel = color.RGBAModel
		}
		return
	}

	d.mode = mNRGBA
	if d.bpp == 16 {
		d.config.ColorModel = color.NRGBA64Model
	} else {
		d.config.ColorModel = color.NRGBAModel
	}
}

// ifdUint decodes the IFD entry in p, which must be of the Byte, Short
// or Long type, and returns the decoded uint values.
//
// maxCount limits the number of values. If the entry contains more than
// maxCount values, only the first maxCount are parsed.
func (d *decoder) ifdUint(p []byte, maxCount int) (u []uint, err error) {
	var raw []byte
	if len(p) < ifdLen {
		return nil, FormatError("bad IFD entry")
	}

	datatype := d.byteOrder.Uint16(p[2:4])
	if dt := int(datatype); dt <= 0 || dt >= len(lengths) {
		return nil, UnsupportedError("IFD entry datatype")
	}

	count := d.byteOrder.Uint32(p[4:8])
	if count > math.MaxInt32/lengths[datatype] {
		return nil, FormatError("IFD data too large")
	}
	truncatedCount := minInt(int(count), maxCount)
	if datalen := lengths[datatype] * count; datalen > 4 {
		truncatedLen := uint64(lengths[datatype]) * uint64(truncatedCount)
		// The IFD contains a pointer to the real value.
		raw, err = safeReadAt(d.r, truncatedLen, int64(d.byteOrder.Uint32(p[8:12])))
	} else {
		raw = p[8 : 8+datalen]
	}
	if err != nil {
		return nil, err
	}

	u = make([]uint, truncatedCount)
	switch datatype {
	case dtByte:
		for i := range u {
			u[i] = uint(raw[i])
		}
	case dtShort:
		for i := range u {
			u[i] = uint(d.byteOrder.Uint16(raw[2*i : 2*(i+1)]))
		}
	case dtLong:
		for i := range u {
			u[i] = uint(d.byteOrder.Uint32(raw[4*i : 4*(i+1)]))
		}
	default:
		return nil, UnsupportedError("data type")
	}
	return u, nil
}

// parseIFDOffsets parses an IFD entry stored in d.ifd using ifdUint.
func (d *decoder) parseIFDOffsets(tag int, maxCount int) ([]uint, error) {
	p, ok := d.ifd[tag]
	if !ok {
		return nil, nil
	}
	return d.ifdUint(p[:], maxCount)
}

// parseIFD decides whether the the IFD entry in p is "interesting" and
// stows away the data in the decoder. It returns the tag number of the
// entry and an error, if any.
func (d *decoder) parseIFD(p []byte) (int, error) {
	tag := d.byteOrder.Uint16(p[0:2])
	const smallEntryMaxCount = 32
	switch tag {
	case tBitsPerSample,
		tExtraSamples,
		tPhotometricInterpretation,
		tCompression,
		tPredictor,
		tRowsPerStrip,
		tTileWidth,
		tTileLength,
		tImageLength,
		tImageWidth,
		tFillOrder,
		tT4Options,
		tT6Options:
		val, err := d.ifdUint(p, smallEntryMaxCount)
		if err != nil {
			return 0, err
		}
		d.features[int(tag)] = val
	case tStripOffsets,
		tStripByteCounts,
		tTileOffsets,
		tTileByteCounts:
		var entry [ifdLen]byte
		copy(entry[:], p)
		d.ifd[int(tag)] = entry
	case tColorMap:
		if err := d.parseIFDColorMap(p); err != nil {
			return 0, err
		}
	case tSampleFormat:
		if err := d.parseIFDSampleFormat(p); err != nil {
			return 0, err
		}
	case tJPEGTables:
		if err := d.parseIFDJPEGTables(p); err != nil {
			return 0, err
		}
	}

	return int(tag), nil
}

func (d *decoder) parseIFDColorMap(p []byte) error {
	val, err := d.ifdUint(p, 3*256+1)
	if err != nil {
		return err
	}
	numcolors := len(val) / 3
	if len(val)%3 != 0 || numcolors <= 0 || numcolors > 256 {
		return FormatError("bad ColorMap length")
	}
	d.palette = make([]color.Color, numcolors)
	for i := 0; i < numcolors; i++ {
		d.palette[i] = color.RGBA64{
			uint16(val[i]),
			uint16(val[i+numcolors]),
			uint16(val[i+2*numcolors]),
			0xffff,
		}
	}
	return nil
}

func (d *decoder) parseIFDSampleFormat(p []byte) error {
	// Page 27 of the spec: If the SampleFormat is present and
	// the value is not 1 [= unsigned integer data], a Baseline
	// TIFF reader that cannot handle the SampleFormat value
	// must terminate the import process gracefully.
	val, err := d.ifdUint(p, 32)
	if err != nil {
		return err
	}
	for _, v := range val {
		if v != 1 {
			return UnsupportedError("sample format")
		}
	}
	return nil
}

func (d *decoder) parseIFDJPEGTables(p []byte) error {
	datatype := d.byteOrder.Uint16(p[2:4])
	if dt := int(datatype); dt != 7 {
		return UnsupportedError("IFD entry datatype")
	}
	size := d.byteOrder.Uint32(p[4:8])
	if size < 4 {
		return FormatError("bad JPEG tables")
	}
	var err error
	d.tables, err = safeReadAt(d.r, uint64(size-4), int64(d.byteOrder.Uint32(p[8:12]))+2)
	return err
}

// readBits reads n bits from the internal buffer starting at the current offset.
func (d *decoder) readBits(n uint) (v uint32, ok bool) {
	for d.nbits < n {
		d.v <<= 8
		if d.off >= len(d.buf) {
			return 0, false
		}
		d.v |= uint32(d.buf[d.off])
		d.off++
		d.nbits += 8
	}
	d.nbits -= n
	rv := d.v >> d.nbits
	d.v &^= rv << d.nbits
	return rv, true
}

// flushBits discards the unread bits in the buffer used by readBits.
// It is used at the end of a line.
func (d *decoder) flushBits() {
	d.v = 0
	d.nbits = 0
}

// minInt returns the smaller of x or y.
func minInt(a, b int) int {
	if a <= b {
		return a
	}
	return b
}

func maxInt() int {
	return int(^uint(0) >> 1)
}

// maxBytesPerPixel is the maximum possible bytes-per-pixel, used for conservative bounds checking.
const maxBytesPerPixel = 8

func checkedMul3(a, b, c int) (int64, bool) {
	if a < 0 || b < 0 || c < 0 {
		return 0, false
	}
	if a != 0 && b > maxInt()/a {
		return 0, false
	}
	ab := a * b
	if ab != 0 && c > maxInt()/ab {
		return 0, false
	}
	return int64(ab * c), true
}

func (d *decoder) applyPredictor16(xmin, ymin, xmax, ymax int) error {
	var off int
	n := 2 * len(d.features[tBitsPerSample]) // bytes per sample times samples per pixel
	for y := ymin; y < ymax; y++ {
		off += n
		for x := 0; x < (xmax-xmin-1)*n; x += 2 {
			if off+2 > len(d.buf) {
				return errNoPixels
			}
			v0 := d.byteOrder.Uint16(d.buf[off-n : off-n+2])
			v1 := d.byteOrder.Uint16(d.buf[off : off+2])
			d.byteOrder.PutUint16(d.buf[off:off+2], v1+v0)
			off += 2
		}
	}
	return nil
}

func (d *decoder) applyPredictor8(xmin, ymin, xmax, ymax int) error {
	var off int
	n := len(d.features[tBitsPerSample]) // bytes per sample times samples per pixel
	for y := ymin; y < ymax; y++ {
		off += n
		for x := 0; x < (xmax-xmin-1)*n; x++ {
			if off >= len(d.buf) {
				return errNoPixels
			}
			d.buf[off] += d.buf[off-n]
			off++
		}
	}
	return nil
}

// applyPredictor reverses the horizontal differences in d.buf; see pages 64-65 of the specification.
func (d *decoder) applyPredictor(xmin, ymin, xmax, ymax int) error {
	if d.firstVal(tPredictor) != prHorizontal {
		return nil
	}
	switch d.bpp {
	case 16:
		return d.applyPredictor16(xmin, ymin, xmax, ymax)
	case 8:
		return d.applyPredictor8(xmin, ymin, xmax, ymax)
	case 1:
		return UnsupportedError("horizontal predictor with 1 BitsPerSample")
	}
	return nil
}

type decodeRegion struct {
	xmin, ymin, xmax, rMaxX, rMaxY, rowBytes int
}

func (d *decoder) decodeGrayPixels(dst image.Image, region decodeRegion) error {
	if d.bpp == 16 {
		img := dst.(*image.Gray16)
		for y := region.ymin; y < region.rMaxY; y++ {
			for x := region.xmin; x < region.rMaxX; x++ {
				if d.off+2 > len(d.buf) {
					return errNoPixels
				}
				v := d.byteOrder.Uint16(d.buf[d.off : d.off+2])
				d.off += 2
				if d.mode == mGrayInvert {
					v = 0xffff - v
				}
				img.SetGray16(x, y, color.Gray16{v})
			}
			if region.rMaxX == img.Bounds().Max.X {
				d.off += 2 * (region.xmax - img.Bounds().Max.X)
			}
		}
	} else {
		img := dst.(*image.Gray)
		max := uint32((1 << d.bpp) - 1)
		for y := region.ymin; y < region.rMaxY; y++ {
			d.off = (y - region.ymin) * region.rowBytes
			d.flushBits()
			for x := region.xmin; x < region.rMaxX; x++ {
				v, ok := d.readBits(d.bpp)
				if !ok {
					return errNoPixels
				}
				v = v * 0xff / max
				if d.mode == mGrayInvert {
					v = 0xff - v
				}
				img.SetGray(x, y, color.Gray{uint8(v)})
			}
		}
	}
	return nil
}

func (d *decoder) decodePalettedPixels(dst image.Image, region decodeRegion) error {
	img := dst.(*image.Paletted)
	for y := region.ymin; y < region.rMaxY; y++ {
		d.off = (y - region.ymin) * region.rowBytes
		d.flushBits()
		for x := region.xmin; x < region.rMaxX; x++ {
			v, ok := d.readBits(d.bpp)
			if !ok {
				return errNoPixels
			}
			img.SetColorIndex(x, y, uint8(v))
		}
	}
	return nil
}

func (d *decoder) decodeRGBPixels(dst image.Image, region decodeRegion) error {
	if d.bpp == 16 {
		img := dst.(*image.RGBA64)
		for y := region.ymin; y < region.rMaxY; y++ {
			d.off = (y - region.ymin) * region.rowBytes * 3
			for x := region.xmin; x < region.rMaxX; x++ {
				if d.off+6 > len(d.buf) {
					return errNoPixels
				}
				r := d.byteOrder.Uint16(d.buf[d.off+0 : d.off+2])
				g := d.byteOrder.Uint16(d.buf[d.off+2 : d.off+4])
				b := d.byteOrder.Uint16(d.buf[d.off+4 : d.off+6])
				d.off += 6
				img.SetRGBA64(x, y, color.RGBA64{r, g, b, 0xffff})
			}
		}
	} else {
		img := dst.(*image.RGBA)
		for y := region.ymin; y < region.rMaxY; y++ {
			min := img.PixOffset(region.xmin, y)
			max := img.PixOffset(region.rMaxX, y)
			off := (y - region.ymin) * (region.xmax - region.xmin) * 3
			for i := min; i < max; i += 4 {
				if off+3 > len(d.buf) {
					return errNoPixels
				}
				img.Pix[i+0] = d.buf[off+0]
				img.Pix[i+1] = d.buf[off+1]
				img.Pix[i+2] = d.buf[off+2]
				img.Pix[i+3] = 0xff
				off += 3
			}
		}
	}
	return nil
}

func (d *decoder) decodeNRGBAPixels(dst image.Image, region decodeRegion) error {
	if d.bpp == 16 {
		img := dst.(*image.NRGBA64)
		for y := region.ymin; y < region.rMaxY; y++ {
			d.off = (y - region.ymin) * region.rowBytes * 4
			for x := region.xmin; x < region.rMaxX; x++ {
				if d.off+8 > len(d.buf) {
					return errNoPixels
				}
				r := d.byteOrder.Uint16(d.buf[d.off+0 : d.off+2])
				g := d.byteOrder.Uint16(d.buf[d.off+2 : d.off+4])
				b := d.byteOrder.Uint16(d.buf[d.off+4 : d.off+6])
				a := d.byteOrder.Uint16(d.buf[d.off+6 : d.off+8])
				d.off += 8
				img.SetNRGBA64(x, y, color.NRGBA64{r, g, b, a})
			}
		}
	} else {
		img := dst.(*image.NRGBA)
		for y := region.ymin; y < region.rMaxY; y++ {
			min := img.PixOffset(region.xmin, y)
			max := img.PixOffset(region.rMaxX, y)
			i0, i1 := (y-region.ymin)*(region.xmax-region.xmin)*4, (y-region.ymin+1)*(region.xmax-region.xmin)*4
			if i1 > len(d.buf) {
				return errNoPixels
			}
			copy(img.Pix[min:max], d.buf[i0:i1])
		}
	}
	return nil
}

func (d *decoder) decodeRGBAPixels(dst image.Image, region decodeRegion) error {
	if d.bpp == 16 {
		img := dst.(*image.RGBA64)
		for y := region.ymin; y < region.rMaxY; y++ {
			d.off = (y - region.ymin) * region.rowBytes * 4
			for x := region.xmin; x < region.rMaxX; x++ {
				if d.off+8 > len(d.buf) {
					return errNoPixels
				}
				r := d.byteOrder.Uint16(d.buf[d.off+0 : d.off+2])
				g := d.byteOrder.Uint16(d.buf[d.off+2 : d.off+4])
				b := d.byteOrder.Uint16(d.buf[d.off+4 : d.off+6])
				a := d.byteOrder.Uint16(d.buf[d.off+6 : d.off+8])
				d.off += 8
				img.SetRGBA64(x, y, color.RGBA64{r, g, b, a})
			}
		}
	} else {
		img := dst.(*image.RGBA)
		for y := region.ymin; y < region.rMaxY; y++ {
			min := img.PixOffset(region.xmin, y)
			max := img.PixOffset(region.rMaxX, y)
			i0, i1 := (y-region.ymin)*(region.xmax-region.xmin)*4, (y-region.ymin+1)*(region.xmax-region.xmin)*4
			if i1 > len(d.buf) {
				return errNoPixels
			}
			copy(img.Pix[min:max], d.buf[i0:i1])
		}
	}
	return nil
}

func (d *decoder) decodeCMYKPixels(dst image.Image, region decodeRegion) error {
	// d.bpp must be 8
	img := dst.(*image.CMYK)
	for y := region.ymin; y < region.rMaxY; y++ {
		min := img.PixOffset(region.xmin, y)
		max := img.PixOffset(region.rMaxX, y)
		i0, i1 := (y-region.ymin)*(region.xmax-region.xmin)*4, (y-region.ymin+1)*(region.xmax-region.xmin)*4
		if i1 > len(d.buf) {
			return errNoPixels
		}
		copy(img.Pix[min:max], d.buf[i0:i1])
	}
	return nil
}

// decode decodes the raw data of an image.
// It reads from d.buf and writes the strip or tile into dst.
func (d *decoder) decode(dst image.Image, xmin, ymin, xmax, ymax int) error {
	d.off = 0
	if err := d.applyPredictor(xmin, ymin, xmax, ymax); err != nil {
		return err
	}

	// Bytes in a full-width, single-component row, including right-edge tile padding.
	region := decodeRegion{
		xmin: xmin, ymin: ymin, xmax: xmax,
		rMaxX:    minInt(xmax, dst.Bounds().Max.X),
		rMaxY:    minInt(ymax, dst.Bounds().Max.Y),
		rowBytes: int((int64(xmax-xmin)*int64(d.bpp) + 7) / 8),
	}
	switch d.mode {
	case mGray, mGrayInvert:
		return d.decodeGrayPixels(dst, region)
	case mPaletted:
		return d.decodePalettedPixels(dst, region)
	case mRGB:
		return d.decodeRGBPixels(dst, region)
	case mNRGBA:
		return d.decodeNRGBAPixels(dst, region)
	case mRGBA:
		return d.decodeRGBAPixels(dst, region)
	case mCMYK:
		return d.decodeCMYKPixels(dst, region)
	}

	return nil
}

func newDecoderAt(r io.Reader, ifdOffset int64) (*decoder, error) {
	d := &decoder{
		r:        newReaderAt(r),
		features: make(map[int][]uint),
		ifd:      make(map[int][ifdLen]byte),
	}

	if err := d.readImageIFD(ifdOffset); err != nil {
		return nil, err
	}
	if err := d.initConfig(); err != nil {
		return nil, err
	}
	return d, nil
}

// DecodeConfig returns the color model and dimensions of a TIFF image without decoding the entire image.
func DecodeConfig(r io.Reader) (image.Config, error) {
	d, err := newDecoderAt(r, 0)
	if err != nil {
		return image.Config{}, err
	}
	return d.config, nil
}

// DecodeConfigAt returns the color model and dimensions of a TIFF image at ifdOffset without decoding the entire image.
func DecodeConfigAt(r io.Reader, ifdOffset int64) (image.Config, error) {
	d, err := newDecoderAt(r, ifdOffset)
	if err != nil {
		return image.Config{}, err
	}
	return d.config, nil
}

func ccittFillOrder(tiffFillOrder uint) ccitt.Order {
	if tiffFillOrder == 2 {
		return ccitt.LSB
	}
	return ccitt.MSB
}

func (d *decoder) ccittOptions(compression uint) (*ccitt.Options, error) {
	opts := &ccitt.Options{
		Invert: d.firstVal(tPhotometricInterpretation) == pWhiteIsZero,
	}
	switch compression {
	case cG3:
		t4Options := d.firstVal(tT4Options)
		if t4Options&1 != 0 {
			return nil, UnsupportedError("CCITT Group 3 2-D coding")
		}
		if t4Options&2 != 0 {
			return nil, UnsupportedError("CCITT Group 3 uncompressed mode")
		}
		opts.Align = t4Options&4 != 0
	case cG4:
		if d.firstVal(tT6Options)&2 != 0 {
			return nil, UnsupportedError("CCITT Group 4 uncompressed mode")
		}
	}
	return opts, nil
}

func (d *decoder) copyStrip(dst image.Image, strip image.Image, x, y int) {
	switch d.mode {
	case mGray, mGrayInvert:
		if d.bpp == 16 {
			draw.Draw(dst.(*image.Gray16), image.Rect(x, y, x+strip.Bounds().Dx(), y+strip.Bounds().Dy()), strip, image.Point{}, draw.Over)
		} else {
			draw.Draw(dst.(*image.Gray), image.Rect(x, y, x+strip.Bounds().Dx(), y+strip.Bounds().Dy()), strip, image.Point{}, draw.Over)
		}
	case mPaletted:
		draw.Draw(dst.(*image.Paletted), image.Rect(x, y, x+strip.Bounds().Dx(), y+strip.Bounds().Dy()), strip, image.Point{}, draw.Over)
	case mNRGBA:
		if d.bpp == 16 {
			draw.Draw(dst.(*image.NRGBA64), image.Rect(x, y, x+strip.Bounds().Dx(), y+strip.Bounds().Dy()), strip, image.Point{}, draw.Over)
		} else {
			draw.Draw(dst.(*image.NRGBA), image.Rect(x, y, x+strip.Bounds().Dx(), y+strip.Bounds().Dy()), strip, image.Point{}, draw.Over)
		}
	case mRGB, mRGBA:
		if d.bpp == 16 {
			draw.Draw(dst.(*image.RGBA64), image.Rect(x, y, x+strip.Bounds().Dx(), y+strip.Bounds().Dy()), strip, image.Point{}, draw.Over)
		} else {
			draw.Draw(dst.(*image.RGBA), image.Rect(x, y, x+strip.Bounds().Dx(), y+strip.Bounds().Dy()), strip, image.Point{}, draw.Over)
		}
	case mCMYK:
		draw.Draw(dst.(*image.CMYK), image.Rect(x, y, x+strip.Bounds().Dx(), y+strip.Bounds().Dy()), strip, image.Point{}, draw.Over)
	}
}

func (d *decoder) prepJPEG(offset, n, lim int64, needAPP14, needTables bool) (io.ReadSeeker, error) {
	bb, err := safeReadAt(d.r, uint64(n), offset)
	if err != nil {
		return nil, err
	}
	if len(bb) < 2 || bb[0] != 0xFF || bb[1] != 0xD8 {
		return nil, FormatError("corrupt JPG stream")
	}

	if needAPP14 {
		var buf bytes.Buffer
		buf.Write(bb[:2])
		buf.Write(app14Marker)
		buf.Write(bb[2:])
		bb = buf.Bytes()
	}

	if needTables {
		sosIndex := bytes.Index(bb, []byte{0xFF, 0xDA})
		if sosIndex == -1 {
			return nil, FormatError("corrupt JPG stream")
		}
		var buf bytes.Buffer
		buf.Write(bb[:sosIndex])
		buf.Write(d.tables)
		buf.Write(bb[sosIndex:])
		bb = buf.Bytes()
	}

	// JPEG headers can exceed the uncompressed block size; bound decoded dimensions instead.
	c, err := jpeg.DecodeConfig(bytes.NewReader(bb))
	if err != nil {
		return nil, err
	}
	size, ok := checkedMul3(c.Width, c.Height, maxBytesPerPixel)
	if !ok || size > lim {
		return nil, FormatError("JPEG dimensions exceed TIFF block size")
	}
	return bytes.NewReader(bb), nil
}

func (d *decoder) decodeJPEGBlock(img image.Image, rd io.ReadSeeker, x, y, width, height int) error {
	c, err := jpeg.DecodeConfig(rd)
	if err != nil {
		return err
	}
	limit, ok := checkedMul3(width, height, maxBytesPerPixel)
	if !ok || c.Width <= 0 || c.Height <= 0 {
		return FormatError("invalid JPEG block dimensions")
	}
	size, ok := checkedMul3(c.Width, c.Height, maxBytesPerPixel)
	if !ok || size > limit {
		return FormatError("JPEG dimensions exceed TIFF block size")
	}
	if c.Width != width || c.Height != height {
		return FormatError("JPEG dimensions do not match TIFF block")
	}
	if _, err := rd.Seek(0, io.SeekStart); err != nil {
		return err
	}
	decoded, _, err := image.Decode(rd)
	if err != nil {
		return err
	}
	d.copyStrip(img, decoded, x, y)
	return nil
}

func (d *decoder) decodeJPGOld(img image.Image, offset, n, lim int64, x, y, width, height int) error {
	var rd io.ReadSeeker = io.NewSectionReader(d.r, offset, n)
	if len(d.features[tBitsPerSample]) == 4 && d.firstVal(tExtraSamples) > 0 {
		var err error
		rd, err = d.prepJPEG(offset, n, lim, true, false)
		if err != nil {
			return err
		}
	}
	return d.decodeJPEGBlock(img, rd, x, y, width, height)
}

type blockLayout struct {
	width, height, across, down int
	padding                     bool
	offsets, counts             []uint
}

type tiffBlock struct {
	offset, n           int64
	x, y, width, height int
}

func (d *decoder) validateTileSize(width, height int) error {
	if width < 8 || height < 8 {
		return FormatError("tile size is too small")
	}
	if _, ok := checkedMul3(width, height, maxBytesPerPixel); !ok {
		return FormatError("tile size is too large")
	}
	if width-d.config.Width > 16 || height-d.config.Height > 16 {
		if width > 1024 || height > 1024 {
			return FormatError("tile size exceeds image size")
		}
	}
	return nil
}

func (d *decoder) tileLayout() (blockLayout, error) {
	l := blockLayout{padding: true}
	var err error
	if l.width, err = d.firstIntVal(tTileWidth); err != nil {
		return l, err
	}
	if l.height, err = d.firstIntVal(tTileLength); err != nil {
		return l, err
	}
	if err := d.validateTileSize(l.width, l.height); err != nil {
		return l, err
	}
	l.across = (d.config.Width + l.width - 1) / l.width
	l.down = (d.config.Height + l.height - 1) / l.height
	if l.offsets, err = d.parseIFDOffsets(tTileOffsets, l.across*l.down); err != nil {
		return l, err
	}
	l.counts, err = d.parseIFDOffsets(tTileByteCounts, l.across*l.down)
	return l, err
}

func (d *decoder) stripLayout() (blockLayout, error) {
	l := blockLayout{width: d.config.Width, height: d.config.Height, across: 1, down: 1}
	if d.config.Width == 0 {
		l.across = 0
	}
	if d.config.Height == 0 {
		l.down = 0
	}
	if v := d.firstVal(tRowsPerStrip); v > 0 && v < uint(l.height) {
		l.height = int(v)
	}
	if l.height != 0 {
		l.down = (d.config.Height + l.height - 1) / l.height
	}
	var err error
	if l.offsets, err = d.parseIFDOffsets(tStripOffsets, l.down); err != nil {
		return l, err
	}
	l.counts, err = d.parseIFDOffsets(tStripByteCounts, l.down)
	return l, err
}

func (d *decoder) readBlockLayout() (blockLayout, error) {
	var l blockLayout
	var err error
	if d.firstVal(tTileWidth) != 0 {
		l, err = d.tileLayout()
	} else {
		l, err = d.stripLayout()
	}
	if err != nil {
		return l, err
	}
	// Check if we have the right number of strips/tiles, offsets and counts.
	if n := l.across * l.down; len(l.offsets) < n || len(l.counts) < n {
		return l, FormatError("inconsistent header")
	}
	return l, nil
}

func (d *decoder) newImage() image.Image {
	rect := image.Rect(0, 0, d.config.Width, d.config.Height)
	switch d.mode {
	case mGray, mGrayInvert:
		if d.bpp == 16 {
			return image.NewGray16(rect)
		}
		return image.NewGray(rect)
	case mPaletted:
		return image.NewPaletted(rect, d.palette)
	case mNRGBA:
		if d.bpp == 16 {
			return image.NewNRGBA64(rect)
		}
		return image.NewNRGBA(rect)
	case mRGB, mRGBA:
		if d.bpp == 16 {
			return image.NewRGBA64(rect)
		}
		return image.NewRGBA(rect)
	case mCMYK:
		return image.NewCMYK(rect)
	}
	return nil
}

func (d *decoder) loadUncompressedBlock(b tiffBlock, limit int64) error {
	if b.n > limit {
		return FormatError("block data size too large")
	}
	var err error
	if r, ok := d.r.(*buffer); ok {
		d.buf, err = r.Slice(int(b.offset), int(b.n))
	} else {
		d.buf, err = safeReadAt(d.r, uint64(b.n), b.offset)
	}
	return err
}

func (d *decoder) loadRLEBlock(b tiffBlock, limit int64) error {
	if d.bpp != 1 {
		return UnsupportedError("CCITT RLE with BitsPerSample != 1")
	}
	order := ccittFillOrder(d.firstVal(tFillOrder))
	whiteIsZero := d.firstVal(tPhotometricInterpretation) == pWhiteIsZero
	var err error
	d.buf, err = decodeCCITTRLE(io.NewSectionReader(d.r, b.offset, b.n), order, b.width, b.height, whiteIsZero, limit)
	return err
}

func (d *decoder) loadFaxBlock(b tiffBlock, limit int64, compression uint) error {
	opts, err := d.ccittOptions(compression)
	if err != nil {
		return err
	}
	// LSBToMSB := d.firstVal(tFillOrder) == 2
	// order := ccitt.MSB
	// if LSBToMSB {
	// 	order = ccitt.LSB
	// }
	order := ccittFillOrder(d.firstVal(tFillOrder))
	subFormat := ccitt.Group3
	if compression == cG4 {
		subFormat = ccitt.Group4
	}
	r := ccitt.NewReader(io.NewSectionReader(d.r, b.offset, b.n), order, subFormat, b.width, b.height, opts)
	d.buf, err = readBuf(r, d.buf, limit)
	return err
}

func (d *decoder) loadLZWBlock(b tiffBlock, limit int64) error {
	r := lzw.NewReader(io.NewSectionReader(d.r, b.offset, b.n))
	var err error
	d.buf, err = readBuf(r, d.buf, d.lzwBlockDataSize(b, limit))
	r.Close()
	return err
}

func (d *decoder) lzwBlockDataSize(b tiffBlock, limit int64) int64 {
	rowBits, ok := checkedMul3(b.width, len(d.features[tBitsPerSample]), int(d.bpp))
	if !ok || rowBits <= 0 || rowBits > int64(maxInt()-7) {
		return limit
	}
	rowBytes := (rowBits + 7) / 8
	if b.height <= 0 || int64(b.height) > limit/rowBytes {
		return limit
	}
	return rowBytes * int64(b.height)
}

func (d *decoder) loadDeflateBlock(b tiffBlock, limit int64) error {
	r, err := zlib.NewReader(io.NewSectionReader(d.r, b.offset, b.n))
	if err != nil {
		return err
	}
	d.buf, err = readBuf(r, d.buf, limit)
	r.Close()
	return err
}

func (d *decoder) loadBlockData(b tiffBlock, limit int64, compression uint) error {
	switch compression {
	// According to the spec, Compression does not have a default value,
	// but some tools interpret a missing Compression value as none so we do
	// the same.
	case cNone, 0:
		return d.loadUncompressedBlock(b, limit)
	case cCCITT:
		return d.loadRLEBlock(b, limit)
	case cG3, cG4:
		return d.loadFaxBlock(b, limit, compression)
	case cLZW:
		return d.loadLZWBlock(b, limit)
	case cDeflate, cDeflateOld:
		return d.loadDeflateBlock(b, limit)
	case cPackBits:
		var err error
		d.buf, err = unpackBits(io.NewSectionReader(d.r, b.offset, b.n), limit)
		return err
	default:
		return UnsupportedError(fmt.Sprintf("compression value %d", compression))
	}
}

func (d *decoder) decodeCompressedJPEG(img image.Image, b tiffBlock, limit int64, compression uint) error {
	if compression == cJPEGOld || len(d.tables) == 0 {
		return d.decodeJPGOld(img, b.offset, b.n, limit, b.x, b.y, b.width, b.height)
	}
	needAPP14 := len(d.features[tBitsPerSample]) == 4 && d.firstVal(tExtraSamples) > 0
	rd, err := d.prepJPEG(b.offset, b.n, limit, needAPP14, true)
	if err != nil {
		return err
	}
	return d.decodeJPEGBlock(img, rd, b.x, b.y, b.width, b.height)
}

func (d *decoder) decodeBlock(img image.Image, b tiffBlock, limit int64) error {
	compression := d.firstVal(tCompression)
	if compression == cJPEGOld || compression == cJPEG {
		return d.decodeCompressedJPEG(img, b, limit, compression)
	}
	if err := d.loadBlockData(b, limit, compression); err != nil {
		return err
	}
	return d.decode(img, b.x, b.y, b.x+b.width, b.y+b.height)
}

func (d *decoder) decodeBlocks(img image.Image, l blockLayout, limit int64) error {
	for i := 0; i < l.across; i++ {
		width := l.width
		if !l.padding && i == l.across-1 && d.config.Width%l.width != 0 {
			width = d.config.Width % l.width
		}
		for j := 0; j < l.down; j++ {
			height := l.height
			if !l.padding && j == l.down-1 && d.config.Height%l.height != 0 {
				height = d.config.Height % l.height
			}
			index := j*l.across + i
			b := tiffBlock{
				offset: int64(l.offsets[index]), n: int64(l.counts[index]),
				x: i * l.width, y: j * l.height, width: width, height: height,
			}
			if err := d.decodeBlock(img, b, limit); err != nil {
				return err
			}
		}
	}
	return nil
}

func decode(d *decoder) (image.Image, error) {
	l, err := d.readBlockLayout()
	if err != nil {
		return nil, err
	}
	img := d.newImage()
	if l.across == 0 || l.down == 0 {
		return img, nil
	}
	limit, ok := checkedMul3(l.width, l.height, maxBytesPerPixel)
	if !ok {
		return nil, FormatError("block data size too large")
	}
	if err := d.decodeBlocks(img, l, limit); err != nil {
		return nil, err
	}
	return img, nil
}

func readBuf(r io.Reader, buf []byte, lim int64) ([]byte, error) {
	b := bytes.NewBuffer(buf[:0])
	_, err := b.ReadFrom(io.LimitReader(r, lim))
	return b.Bytes(), err
}

// Decode reads a TIFF image from r and returns it as an image.Image.
// The type of Image returned depends on the contents of the TIFF.
func Decode(r io.Reader) (img image.Image, err error) {
	d, err := newDecoderAt(r, 0)
	if err != nil {
		return
	}
	return decodeImage(d)
}

// DecodeAt reads a TIFF image from r at ifdOffset and returns it as an image.Image.
// The type of Image returned depends on the contents of the TIFF.
func DecodeAt(r io.Reader, ifdOffset int64) (img image.Image, err error) {
	d, err := newDecoderAt(r, ifdOffset)
	if err != nil {
		return
	}
	return decodeImage(d)
}

func init() {
	image.RegisterFormat("tiff", leHeader, Decode, DecodeConfig)
	image.RegisterFormat("tiff", beHeader, Decode, DecodeConfig)
}
