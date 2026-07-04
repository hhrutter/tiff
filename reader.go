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
	"io"
	"math"

	"golang.org/x/image/ccitt"
	"golang.org/x/image/tiff/lzw"
)

// A FormatError reports that the input is not a valid TIFF image.
type FormatError string

func (e FormatError) Error() string {
	return "tiff: invalid format: " + string(e)
}

// An UnsupportedError reports that the input uses a valid but
// unimplemented feature.
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
		next := n
		if next > maxChunkSize {
			next = maxChunkSize
		}
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
		val, err := d.ifdUint(p, 3*256+1)
		if err != nil {
			return 0, err
		}
		numcolors := len(val) / 3
		if len(val)%3 != 0 || numcolors <= 0 || numcolors > 256 {
			return 0, FormatError("bad ColorMap length")
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
	case tSampleFormat:
		// Page 27 of the spec: If the SampleFormat is present and
		// the value is not 1 [= unsigned integer data], a Baseline
		// TIFF reader that cannot handle the SampleFormat value
		// must terminate the import process gracefully.
		val, err := d.ifdUint(p, smallEntryMaxCount)
		if err != nil {
			return 0, err
		}
		for _, v := range val {
			if v != 1 {
				return 0, UnsupportedError("sample format")
			}
		}
	case tJPEGTables:
		datatype := d.byteOrder.Uint16(p[2:4])
		if dt := int(datatype); dt != 7 {
			return 0, UnsupportedError("IFD entry datatype")
		}
		size := d.byteOrder.Uint32(p[4:8])
		if size < 4 {
			return 0, FormatError("bad JPEG tables")
		}
		var err error
		d.tables, err = safeReadAt(d.r, uint64(size-4), int64(d.byteOrder.Uint32(p[8:12]))+2)
		if err != nil {
			return 0, err
		}
	}

	return int(tag), nil
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

// maxBytesPerPixel is the maximum possible bytes-per-pixel, used for
// conservative bounds checking.
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

// decode decodes the raw data of an image.
// It reads from d.buf and writes the strip or tile into dst.
func (d *decoder) decode(dst image.Image, xmin, ymin, xmax, ymax int) error {
	d.off = 0

	// Apply horizontal predictor if necessary.
	// In this case, p contains the color difference to the preceding pixel.
	// See page 64-65 of the spec.
	if d.firstVal(tPredictor) == prHorizontal {
		switch d.bpp {
		case 16:
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
		case 8:
			var off int
			n := 1 * len(d.features[tBitsPerSample]) // bytes per sample times samples per pixel
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
		case 1:
			return UnsupportedError("horizontal predictor with 1 BitsPerSample")
		}
	}

	rMaxX := minInt(xmax, dst.Bounds().Max.X)
	rMaxY := minInt(ymax, dst.Bounds().Max.Y)
	switch d.mode {
	case mGray, mGrayInvert:
		if d.bpp == 16 {
			img := dst.(*image.Gray16)
			for y := ymin; y < rMaxY; y++ {
				for x := xmin; x < rMaxX; x++ {
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
				if rMaxX == img.Bounds().Max.X {
					d.off += 2 * (xmax - img.Bounds().Max.X)
				}
			}
		} else {
			img := dst.(*image.Gray)
			max := uint32((1 << d.bpp) - 1)
			for y := ymin; y < rMaxY; y++ {
				for x := xmin; x < rMaxX; x++ {
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
				d.flushBits()
			}
		}
	case mPaletted:
		img := dst.(*image.Paletted)
		for y := ymin; y < rMaxY; y++ {
			for x := xmin; x < rMaxX; x++ {
				v, ok := d.readBits(d.bpp)
				if !ok {
					return errNoPixels
				}
				img.SetColorIndex(x, y, uint8(v))
			}
			d.flushBits()
		}
	case mRGB:
		if d.bpp == 16 {
			img := dst.(*image.RGBA64)
			for y := ymin; y < rMaxY; y++ {
				for x := xmin; x < rMaxX; x++ {
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
			for y := ymin; y < rMaxY; y++ {
				min := img.PixOffset(xmin, y)
				max := img.PixOffset(rMaxX, y)
				off := (y - ymin) * (xmax - xmin) * 3
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
	case mNRGBA:
		if d.bpp == 16 {
			img := dst.(*image.NRGBA64)
			for y := ymin; y < rMaxY; y++ {
				for x := xmin; x < rMaxX; x++ {
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
			for y := ymin; y < rMaxY; y++ {
				min := img.PixOffset(xmin, y)
				max := img.PixOffset(rMaxX, y)
				i0, i1 := (y-ymin)*(xmax-xmin)*4, (y-ymin+1)*(xmax-xmin)*4
				if i1 > len(d.buf) {
					return errNoPixels
				}
				copy(img.Pix[min:max], d.buf[i0:i1])
			}
		}
	case mRGBA:
		if d.bpp == 16 {
			img := dst.(*image.RGBA64)
			for y := ymin; y < rMaxY; y++ {
				for x := xmin; x < rMaxX; x++ {
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
			for y := ymin; y < rMaxY; y++ {
				min := img.PixOffset(xmin, y)
				max := img.PixOffset(rMaxX, y)
				i0, i1 := (y-ymin)*(xmax-xmin)*4, (y-ymin+1)*(xmax-xmin)*4
				if i1 > len(d.buf) {
					return errNoPixels
				}
				copy(img.Pix[min:max], d.buf[i0:i1])
			}
		}
	case mCMYK:
		// d.bpp must be 8
		img := dst.(*image.CMYK)
		for y := ymin; y < rMaxY; y++ {
			min := img.PixOffset(xmin, y)
			max := img.PixOffset(rMaxX, y)
			i0, i1 := (y-ymin)*(xmax-xmin)*4, (y-ymin+1)*(xmax-xmin)*4
			if i1 > len(d.buf) {
				return errNoPixels
			}
			copy(img.Pix[min:max], d.buf[i0:i1])
		}

	}

	return nil
}

func newDecoderAt(r io.Reader, ifdOffset int64) (*decoder, error) {
	d := &decoder{
		r:        newReaderAt(r),
		features: make(map[int][]uint),
		ifd:      make(map[int][ifdLen]byte),
	}

	p := make([]byte, 8)
	if _, err := d.r.ReadAt(p, 0); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	switch string(p[0:4]) {
	case leHeader:
		d.byteOrder = binary.LittleEndian
	case beHeader:
		d.byteOrder = binary.BigEndian
	default:
		return nil, FormatError("malformed header")
	}

	if ifdOffset == 0 {
		ifdOffset = int64(d.byteOrder.Uint32(p[4:8]))
	}

	// The first two bytes contain the number of entries (12 bytes each).
	if _, err := d.r.ReadAt(p[0:2], ifdOffset); err != nil {
		return nil, err
	}
	numItems := int(d.byteOrder.Uint16(p[0:2]))

	// All IFD entries are read in one chunk.
	var err error
	p, err = safeReadAt(d.r, uint64(ifdLen*numItems), ifdOffset+2)
	if err != nil {
		return nil, err
	}

	prevTag := -1
	for i := 0; i < len(p); i += ifdLen {
		tag, err := d.parseIFD(p[i : i+ifdLen])
		if err != nil {
			return nil, err
		}
		if tag <= prevTag {
			return nil, FormatError("tags are not sorted in ascending order")
		}
		prevTag = tag
	}

	if d.config.Width, err = d.firstIntVal(tImageWidth); err != nil {
		return nil, err
	}
	if d.config.Height, err = d.firstIntVal(tImageLength); err != nil {
		return nil, err
	}
	if _, ok := checkedMul3(d.config.Width, d.config.Height, maxBytesPerPixel); !ok {
		return nil, FormatError("image too large")
	}

	if _, ok := d.features[tBitsPerSample]; !ok {
		// Default is 1 per specification.
		d.features[tBitsPerSample] = []uint{1}
	}
	d.bpp = d.firstVal(tBitsPerSample)
	switch d.bpp {
	case 0:
		return nil, FormatError("BitsPerSample must not be 0")
	case 1, 8, 16:
		// Nothing to do, these are accepted by this implementation.
	default:
		return nil, UnsupportedError(fmt.Sprintf("BitsPerSample of %v", d.bpp))
	}

	// Determine the image mode.
	switch d.firstVal(tPhotometricInterpretation) {
	case pRGB:
		if d.bpp == 16 {
			for _, b := range d.features[tBitsPerSample] {
				if b != 16 {
					return nil, FormatError("wrong number of samples for 16bit RGB")
				}
			}
		} else {
			for _, b := range d.features[tBitsPerSample] {
				if b != 8 {
					return nil, FormatError("wrong number of samples for 8bit RGB")
				}
			}
		}
		// RGB images normally have 3 samples per pixel.
		// If there are more, ExtraSamples (p. 31-32 of the spec)
		// gives their meaning (usually an alpha channel).
		switch len(d.features[tBitsPerSample]) {
		case 3:
			d.mode = mRGB
			if d.bpp == 16 {
				d.config.ColorModel = color.RGBA64Model
			} else {
				d.config.ColorModel = color.RGBAModel
			}
		case 4:
			switch d.firstVal(tExtraSamples) {
			case 0, 1:
				d.setRGBAMode(true)
			case 2:
				d.setRGBAMode(false)
			default:
				return nil, FormatError("wrong number of samples for RGB")
			}
		default:
			return nil, FormatError("wrong number of samples for RGB")
		}
	case pPaletted:
		d.mode = mPaletted
		d.config.ColorModel = color.Palette(d.palette)
	case pWhiteIsZero:
		d.mode = mGrayInvert
		if d.bpp == 16 {
			d.config.ColorModel = color.Gray16Model
		} else {
			d.config.ColorModel = color.GrayModel
		}
	case pBlackIsZero:
		d.mode = mGray
		if d.bpp == 16 {
			d.config.ColorModel = color.Gray16Model
		} else {
			d.config.ColorModel = color.GrayModel
		}
	case pCMYK:
		d.mode = mCMYK
		if d.bpp == 16 {
			return nil, UnsupportedError(fmt.Sprintf("CMYK BitsPerSample of %v", d.bpp))
		}
		d.config.ColorModel = color.CMYKModel

	default:
		return nil, UnsupportedError("color model")
	}

	return d, nil
}

// DecodeConfig returns the color model and dimensions of a TIFF image without
// decoding the entire image.
func DecodeConfig(r io.Reader) (image.Config, error) {
	d, err := newDecoderAt(r, 0)
	if err != nil {
		return image.Config{}, err
	}
	return d.config, nil
}

// DecodeConfig returns the color model and dimensions of a TIFF image at ifdOffset without
// decoding the entire image.
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

func (d *decoder) prepJPEG(offset, n, lim int64, needAPP14, needTables bool) (io.Reader, error) {
	if n > lim {
		return nil, FormatError("block data size too large")
	}
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

	return bytes.NewReader(bb), nil
}

func (d *decoder) decodeJPGOld(img image.Image, offset, n, lim int64, i, j, blockWidth, blockHeight int) error {
	var rd io.Reader = io.NewSectionReader(d.r, offset, n)
	if len(d.features[tBitsPerSample]) == 4 && d.firstVal(tExtraSamples) > 0 {
		var err error
		rd, err = d.prepJPEG(offset, n, lim, true, false)
		if err != nil {
			return err
		}
	}
	img0, _, err := image.Decode(rd)
	if err != nil {
		return err
	}
	d.copyStrip(img, img0, i*blockWidth, j*blockHeight)
	return nil
}

func decode(d *decoder) (img image.Image, err error) {
	blockPadding := false
	blockWidth := d.config.Width
	blockHeight := d.config.Height
	blocksAcross := 1
	blocksDown := 1

	if d.config.Width == 0 {
		blocksAcross = 0
	}
	if d.config.Height == 0 {
		blocksDown = 0
	}

	var blockOffsets, blockCounts []uint

	if d.firstVal(tTileWidth) != 0 {
		blockPadding = true

		if blockWidth, err = d.firstIntVal(tTileWidth); err != nil {
			return nil, err
		}
		if blockHeight, err = d.firstIntVal(tTileLength); err != nil {
			return nil, err
		}
		if blockWidth < 8 || blockHeight < 8 {
			return nil, FormatError("tile size is too small")
		}
		if _, ok := checkedMul3(blockWidth, blockHeight, maxBytesPerPixel); !ok {
			return nil, FormatError("tile size is too large")
		}
		if blockWidth-d.config.Width > 16 || blockHeight-d.config.Height > 16 {
			if blockWidth > 1024 || blockHeight > 1024 {
				return nil, FormatError("tile size exceeds image size")
			}
		}

		if blockWidth != 0 {
			blocksAcross = (d.config.Width + blockWidth - 1) / blockWidth
		}
		if blockHeight != 0 {
			blocksDown = (d.config.Height + blockHeight - 1) / blockHeight
		}

		blockOffsets, err = d.parseIFDOffsets(tTileOffsets, blocksAcross*blocksDown)
		if err != nil {
			return nil, err
		}
		blockCounts, err = d.parseIFDOffsets(tTileByteCounts, blocksAcross*blocksDown)
		if err != nil {
			return nil, err
		}

	} else {
		if v := d.firstVal(tRowsPerStrip); v > 0 && v < uint(blockHeight) {
			blockHeight = int(v)
		}

		if blockHeight != 0 {
			blocksDown = (d.config.Height + blockHeight - 1) / blockHeight
		}

		blockOffsets, err = d.parseIFDOffsets(tStripOffsets, blocksDown)
		if err != nil {
			return nil, err
		}
		blockCounts, err = d.parseIFDOffsets(tStripByteCounts, blocksDown)
		if err != nil {
			return nil, err
		}
	}

	// Check if we have the right number of strips/tiles, offsets and counts.
	if n := blocksAcross * blocksDown; len(blockOffsets) < n || len(blockCounts) < n {
		return nil, FormatError("inconsistent header")
	}

	imgRect := image.Rect(0, 0, d.config.Width, d.config.Height)
	switch d.mode {
	case mGray, mGrayInvert:
		if d.bpp == 16 {
			img = image.NewGray16(imgRect)
		} else {
			img = image.NewGray(imgRect)
		}
	case mPaletted:
		img = image.NewPaletted(imgRect, d.palette)
	case mNRGBA:
		if d.bpp == 16 {
			img = image.NewNRGBA64(imgRect)
		} else {
			img = image.NewNRGBA(imgRect)
		}
	case mRGB, mRGBA:
		if d.bpp == 16 {
			img = image.NewRGBA64(imgRect)
		} else {
			img = image.NewRGBA(imgRect)
		}
	case mCMYK:
		img = image.NewCMYK(imgRect)
	}

	if blocksAcross == 0 || blocksDown == 0 {
		return img, nil
	}
	blockMaxDataSize, ok := checkedMul3(blockWidth, blockHeight, maxBytesPerPixel)
	if !ok {
		return nil, FormatError("block data size too large")
	}

	for i := 0; i < blocksAcross; i++ {
		blkW := blockWidth
		if !blockPadding && i == blocksAcross-1 && d.config.Width%blockWidth != 0 {
			blkW = d.config.Width % blockWidth
		}
		for j := 0; j < blocksDown; j++ {
			blkH := blockHeight
			if !blockPadding && j == blocksDown-1 && d.config.Height%blockHeight != 0 {
				blkH = d.config.Height % blockHeight
			}
			offset := int64(blockOffsets[j*blocksAcross+i])
			n := int64(blockCounts[j*blocksAcross+i])
			// LSBToMSB := d.firstVal(tFillOrder) == 2
			// order := ccitt.MSB
			// if LSBToMSB {
			// 	order = ccitt.LSB
			// }
			switch d.firstVal(tCompression) {

			// According to the spec, Compression does not have a default value,
			// but some tools interpret a missing Compression value as none so we do
			// the same.
			case cNone, 0:
				if n > blockMaxDataSize {
					return nil, FormatError("block data size too large")
				}
				if b, ok := d.r.(*buffer); ok {
					d.buf, err = b.Slice(int(offset), int(n))
				} else {
					d.buf, err = safeReadAt(d.r, uint64(n), offset)
				}
			case cCCITT:
				if d.bpp != 1 {
					return nil, UnsupportedError("CCITT RLE with BitsPerSample != 1")
				}
				order := ccittFillOrder(d.firstVal(tFillOrder))
				whiteIsZero := d.firstVal(tPhotometricInterpretation) == pWhiteIsZero
				d.buf, err = decodeCCITTRLE(io.NewSectionReader(d.r, offset, n), order, blkW, blkH, whiteIsZero, blockMaxDataSize)
			case cG3:
				opts, err := d.ccittOptions(cG3)
				if err != nil {
					return nil, err
				}
				order := ccittFillOrder(d.firstVal(tFillOrder))
				r := ccitt.NewReader(io.NewSectionReader(d.r, offset, n), order, ccitt.Group3, blkW, blkH, opts)
				d.buf, err = readBuf(r, d.buf, blockMaxDataSize)
			case cG4:
				opts, err := d.ccittOptions(cG4)
				if err != nil {
					return nil, err
				}
				order := ccittFillOrder(d.firstVal(tFillOrder))
				r := ccitt.NewReader(io.NewSectionReader(d.r, offset, n), order, ccitt.Group4, blkW, blkH, opts)
				d.buf, err = readBuf(r, d.buf, blockMaxDataSize)
			case cLZW:
				r := lzw.NewReader(io.NewSectionReader(d.r, offset, n), lzw.MSB, 8)
				d.buf, err = readBuf(r, d.buf, blockMaxDataSize)
				r.Close()
			case cDeflate, cDeflateOld:
				var r io.ReadCloser
				r, err = zlib.NewReader(io.NewSectionReader(d.r, offset, n))
				if err != nil {
					return nil, err
				}
				d.buf, err = readBuf(r, d.buf, blockMaxDataSize)
				r.Close()
			case cJPEGOld:
				if err := d.decodeJPGOld(img, offset, n, blockMaxDataSize, i, j, blockWidth, blockHeight); err != nil {
					return nil, err
				}
				continue
			case cJPEG:
				if len(d.tables) == 0 {
					if err := d.decodeJPGOld(img, offset, n, blockMaxDataSize, i, j, blockWidth, blockHeight); err != nil {
						return nil, err
					}
					continue
				}
				needAPP14 := len(d.features[tBitsPerSample]) == 4 && d.firstVal(tExtraSamples) > 0
				rd, err := d.prepJPEG(offset, n, blockMaxDataSize, needAPP14, true)
				if err != nil {
					return nil, err
				}
				img0, _, err := image.Decode(rd)
				if err != nil {
					return nil, err
				}
				d.copyStrip(img, img0, i*blockWidth, j*blockHeight)
				continue
			case cPackBits:
				d.buf, err = unpackBits(io.NewSectionReader(d.r, offset, n), blockMaxDataSize)
			default:
				err = UnsupportedError(fmt.Sprintf("compression value %d", d.firstVal(tCompression)))
			}
			if err != nil {
				return nil, err
			}

			xmin := i * blockWidth
			ymin := j * blockHeight
			xmax := xmin + blkW
			ymax := ymin + blkH
			err = d.decode(img, xmin, ymin, xmax, ymax)
			if err != nil {
				return nil, err
			}
		}
	}
	return
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
	return decode(d)
}

// DecodeAt reads a TIFF image from r at ifdOffset and returns it as an image.Image.
// The type of Image returned depends on the contents of the TIFF.
func DecodeAt(r io.Reader, ifdOffset int64) (img image.Image, err error) {
	d, err := newDecoderAt(r, ifdOffset)
	if err != nil {
		return
	}
	return decode(d)
}

func init() {
	image.RegisterFormat("tiff", leHeader, Decode, DecodeConfig)
	image.RegisterFormat("tiff", beHeader, Decode, DecodeConfig)
}
