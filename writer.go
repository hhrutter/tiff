// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tiff

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"sort"
)

// The TIFF format allows to choose the order of the different elements freely.
// The basic structure of a TIFF file written by this package is:
//
//   1. Header (8 bytes).
//   2. Image data.
//   3. Image File Directory (IFD).
//   4. "Pointer area" for larger entries in the IFD.

// We only write little-endian TIFF files.
var enc = binary.LittleEndian

// An ifdEntry is a single entry in an Image File Directory.
// A value of type dtRational is composed of two 32-bit values,
// thus data contains two uints (numerator and denominator) for a single number.
type ifdEntry struct {
	tag      int
	datatype int
	data     []uint32
}

func (e ifdEntry) putData(p []byte) {
	for _, d := range e.data {
		switch e.datatype {
		case dtByte, dtASCII:
			p[0] = byte(d)
			p = p[1:]
		case dtShort:
			enc.PutUint16(p, uint16(d))
			p = p[2:]
		case dtLong, dtRational:
			enc.PutUint32(p, uint32(d))
			p = p[4:]
		}
	}
}

type byTag []ifdEntry

func (d byTag) Len() int           { return len(d) }
func (d byTag) Less(i, j int) bool { return d[i].tag < d[j].tag }
func (d byTag) Swap(i, j int)      { d[i], d[j] = d[j], d[i] }

type encodedImage struct {
	data []byte
	ifd  []ifdEntry
}

func writeBytes(w io.Writer, p []byte) error {
	n, err := w.Write(p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}

func encodeGray(w io.Writer, pix []uint8, dx, dy, stride int, predictor bool) error {
	if dx == 0 || dy == 0 {
		return nil
	}
	if !predictor {
		return writePix(w, pix, dy, dx, stride)
	}
	buf := make([]byte, dx)
	for y := 0; y < dy; y++ {
		min := y*stride + 0
		max := y*stride + dx
		off := 0
		var v0 uint8
		for i := min; i < max; i++ {
			v1 := pix[i]
			buf[off] = v1 - v0
			v0 = v1
			off++
		}
		if err := writeBytes(w, buf); err != nil {
			return err
		}
	}
	return nil
}

func encodeGray16(w io.Writer, pix []uint8, dx, dy, stride int, predictor bool) error {
	if dx == 0 || dy == 0 {
		return nil
	}
	buf := make([]byte, dx*2)
	for y := 0; y < dy; y++ {
		min := y*stride + 0
		max := y*stride + dx*2
		off := 0
		var v0 uint16
		for i := min; i < max; i += 2 {
			// An image.Gray16's Pix is in big-endian order.
			v1 := uint16(pix[i])<<8 | uint16(pix[i+1])
			if predictor {
				v0, v1 = v1, v1-v0
			}
			// We only write little-endian TIFF files.
			buf[off+0] = byte(v1)
			buf[off+1] = byte(v1 >> 8)
			off += 2
		}
		if err := writeBytes(w, buf); err != nil {
			return err
		}
	}
	return nil
}

func encodeRGBA(w io.Writer, pix []uint8, dx, dy, stride int, predictor bool) error {
	if dx == 0 || dy == 0 {
		return nil
	}
	if !predictor {
		return writePix(w, pix, dy, dx*4, stride)
	}
	buf := make([]byte, dx*4)
	for y := 0; y < dy; y++ {
		min := y*stride + 0
		max := y*stride + dx*4
		off := 0
		var r0, g0, b0, a0 uint8
		for i := min; i < max; i += 4 {
			r1, g1, b1, a1 := pix[i+0], pix[i+1], pix[i+2], pix[i+3]
			buf[off+0] = r1 - r0
			buf[off+1] = g1 - g0
			buf[off+2] = b1 - b0
			buf[off+3] = a1 - a0
			off += 4
			r0, g0, b0, a0 = r1, g1, b1, a1
		}
		if err := writeBytes(w, buf); err != nil {
			return err
		}
	}
	return nil
}

func encodeRGBA64(w io.Writer, pix []uint8, dx, dy, stride int, predictor bool) error {
	if dx == 0 || dy == 0 {
		return nil
	}
	buf := make([]byte, dx*8)
	for y := 0; y < dy; y++ {
		min := y*stride + 0
		max := y*stride + dx*8
		off := 0
		var r0, g0, b0, a0 uint16
		for i := min; i < max; i += 8 {
			// An image.RGBA64's Pix is in big-endian order.
			r1 := uint16(pix[i+0])<<8 | uint16(pix[i+1])
			g1 := uint16(pix[i+2])<<8 | uint16(pix[i+3])
			b1 := uint16(pix[i+4])<<8 | uint16(pix[i+5])
			a1 := uint16(pix[i+6])<<8 | uint16(pix[i+7])
			if predictor {
				r0, r1 = r1, r1-r0
				g0, g1 = g1, g1-g0
				b0, b1 = b1, b1-b0
				a0, a1 = a1, a1-a0
			}
			// We only write little-endian TIFF files.
			buf[off+0] = byte(r1)
			buf[off+1] = byte(r1 >> 8)
			buf[off+2] = byte(g1)
			buf[off+3] = byte(g1 >> 8)
			buf[off+4] = byte(b1)
			buf[off+5] = byte(b1 >> 8)
			buf[off+6] = byte(a1)
			buf[off+7] = byte(a1 >> 8)
			off += 8
		}
		if err := writeBytes(w, buf); err != nil {
			return err
		}
	}
	return nil
}

func encodeCMYK(w io.Writer, pix []uint8, dx, dy, stride int, predictor bool) error {
	if dx == 0 || dy == 0 {
		return nil
	}
	if !predictor {
		return writePix(w, pix, dy, dx*4, stride)
	}
	buf := make([]byte, dx*4)
	for y := 0; y < dy; y++ {
		min := y*stride + 0
		max := y*stride + dx*4
		off := 0
		var c0, m0, y0, k0 uint8
		for i := min; i < max; i += 4 {
			c1, m1, y1, k1 := pix[i+0], pix[i+1], pix[i+2], pix[i+3]
			buf[off+0] = c1 - c0
			buf[off+1] = m1 - m0
			buf[off+2] = y1 - y0
			buf[off+3] = k1 - k0
			off += 4
			c0, m0, y0, k0 = c1, m1, y1, k1
		}
		if err := writeBytes(w, buf); err != nil {
			return err
		}
	}
	return nil
}

func encode(w io.Writer, m image.Image, predictor bool) error {
	bounds := m.Bounds()
	if bounds.Min.X == bounds.Max.X || bounds.Min.Y == bounds.Max.Y {
		return nil
	}
	buf := make([]byte, 4*bounds.Dx())
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		off := 0
		if predictor {
			var r0, g0, b0, a0 uint8
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				r, g, b, a := m.At(x, y).RGBA()
				r1 := uint8(r >> 8)
				g1 := uint8(g >> 8)
				b1 := uint8(b >> 8)
				a1 := uint8(a >> 8)
				buf[off+0] = r1 - r0
				buf[off+1] = g1 - g0
				buf[off+2] = b1 - b0
				buf[off+3] = a1 - a0
				off += 4
				r0, g0, b0, a0 = r1, g1, b1, a1
			}
		} else {
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				r, g, b, a := m.At(x, y).RGBA()
				buf[off+0] = uint8(r >> 8)
				buf[off+1] = uint8(g >> 8)
				buf[off+2] = uint8(b >> 8)
				buf[off+3] = uint8(a >> 8)
				off += 4
			}
		}
		if err := writeBytes(w, buf); err != nil {
			return err
		}
	}
	return nil
}

// writePix writes the internal byte array of an image to w. It is less general
// but much faster then encode. writePix is used when pix directly
// corresponds to one of the TIFF image types.
func writePix(w io.Writer, pix []byte, nrows, length, stride int) error {
	if length == stride {
		return writeBytes(w, pix[:nrows*length])
	}
	for ; nrows > 0; nrows-- {
		if err := writeBytes(w, pix[:length]); err != nil {
			return err
		}
		pix = pix[stride:]
	}
	return nil
}

func ifdEntrySize(ent ifdEntry) (uint32, uint64, error) {
	if ent.tag < 0 || ent.tag > math.MaxUint16 {
		return 0, 0, errors.New("tiff: invalid IFD tag")
	}
	if ent.datatype <= 0 || ent.datatype >= len(lengths) {
		return 0, 0, errors.New("tiff: invalid IFD datatype")
	}
	if ent.datatype == dtRational && len(ent.data)%2 != 0 {
		return 0, 0, errors.New("tiff: invalid rational IFD data")
	}
	count := uint64(len(ent.data))
	if ent.datatype == dtRational {
		count /= 2
	}
	if count > math.MaxUint32 {
		return 0, 0, errors.New("tiff: too many IFD values")
	}
	datalen := count * uint64(lengths[ent.datatype])
	if datalen > uint64(maxInt()) {
		return 0, 0, errors.New("tiff: IFD data too large")
	}
	return uint32(count), datalen, nil
}

func marshalIFDEntry(ent ifdEntry, pstart uint64, parea *[]byte) ([ifdLen]byte, error) {
	var buf [ifdLen]byte
	count, datalen, err := ifdEntrySize(ent)
	if err != nil {
		return buf, err
	}
	enc.PutUint16(buf[0:2], uint16(ent.tag))
	enc.PutUint16(buf[2:4], uint16(ent.datatype))
	enc.PutUint32(buf[4:8], count)
	if datalen <= 4 {
		ent.putData(buf[8:12])
		return buf, nil
	}

	pointer := pstart + uint64(len(*parea))
	if err := checkClassicTIFFOffset(pointer); err != nil || datalen > math.MaxUint32-pointer {
		return buf, errors.New("tiff: file too large for classic TIFF offsets")
	}
	o := len(*parea)
	*parea = append(*parea, make([]byte, int(datalen))...)
	ent.putData((*parea)[o:])
	enc.PutUint32(buf[8:12], uint32(pointer))
	return buf, nil
}

func marshalIFD(ifdOffset, nextIFDOffset uint32, d []ifdEntry) ([]byte, error) {
	if len(d) > math.MaxUint16 {
		return nil, errors.New("tiff: too many IFD entries")
	}
	pstart := uint64(ifdOffset) + uint64(ifdLen)*uint64(len(d)) + 6
	if err := checkClassicTIFFOffset(pstart); err != nil {
		return nil, err
	}

	var out bytes.Buffer
	var parea []byte

	// The IFD has to be written with the tags in ascending order.
	d = append([]ifdEntry(nil), d...)
	sort.Sort(byTag(d))

	// Write the number of entries in this IFD.
	if err := binary.Write(&out, enc, uint16(len(d))); err != nil {
		return nil, err
	}
	for _, ent := range d {
		buf, err := marshalIFDEntry(ent, pstart, &parea)
		if err != nil {
			return nil, err
		}
		if _, err := out.Write(buf[:]); err != nil {
			return nil, err
		}
	}
	// The IFD ends with the offset of the next IFD in the file,
	// or zero if it is the last one (page 14).
	if err := binary.Write(&out, enc, nextIFDOffset); err != nil {
		return nil, err
	}
	_, err := out.Write(parea)
	return out.Bytes(), err
}

// Options are the encoding parameters.
type Options struct {
	// Compression is the type of compression used.
	Compression CompressionType
	// Predictor determines whether a differencing predictor is used;
	// if true, instead of each pixel's color, the color difference to the
	// preceding one is saved.  This improves the compression for certain
	// types of images and compressors. For example, it works well for
	// photos with Deflate compression.
	Predictor bool
}

// Encode writes the image m to w. opt determines the options used for
// encoding, such as the compression type. If opt is nil, an uncompressed
// image is written.
func Encode(w io.Writer, m image.Image, opt *Options) error {
	return EncodeAll(w, []image.Image{m}, opt)
}

func compressionOptions(opt *Options) (uint32, bool) {
	compression := uint32(cNone)
	predictor := false
	if opt != nil {
		compression = opt.Compression.specValue()
		// The TIFF 6.0 spec (June, 1992) says the predictor field is only to be used with LZW. (See page 64).
		// Yet this TIFF writer also allows prediction for Deflate compression.
		// This makes sense as Deflate is supposedly the successor to LZW.
		// Also both PNG and PDF use Deflate with predictors.
		predictor = opt.Predictor && (compression == cLZW || compression == cDeflate)
	}
	return compression, predictor
}

func imageDimensions(m image.Image, compression uint32) (image.Point, error) {
	if m == nil || isNilStandardImage(m) {
		return image.Point{}, errors.New("tiff: nil image")
	}
	b := m.Bounds()
	if b.Max.X < b.Min.X || b.Max.Y < b.Min.Y {
		return image.Point{}, errors.New("tiff: invalid image bounds")
	}
	dx := uint64(uint(b.Max.X) - uint(b.Min.X))
	dy := uint64(uint(b.Max.Y) - uint(b.Min.Y))
	if dx > math.MaxUint32 || dy > math.MaxUint32 || dx > uint64(maxInt()) || dy > uint64(maxInt()) {
		return image.Point{}, errors.New("tiff: image dimensions too large")
	}
	d := image.Pt(int(dx), int(dy))
	size, ok := checkedMul3(d.X, d.Y, imageBytesPerPixel(m))
	if !ok || compression == cNone && uint64(size) > math.MaxUint32 {
		return image.Point{}, errors.New("tiff: image data too large")
	}
	if err := validateImagePix(m, d); err != nil {
		return image.Point{}, err
	}
	return d, nil
}

func isNilStandardImage(m image.Image) bool {
	switch m := m.(type) {
	case *image.Paletted:
		return m == nil
	case *image.Gray:
		return m == nil
	case *image.Gray16:
		return m == nil
	case *image.NRGBA:
		return m == nil
	case *image.NRGBA64:
		return m == nil
	case *image.RGBA:
		return m == nil
	case *image.RGBA64:
		return m == nil
	case *image.CMYK:
		return m == nil
	}
	return false
}

func imageBytesPerPixel(m image.Image) int {
	switch m.(type) {
	case *image.Paletted, *image.Gray:
		return 1
	case *image.Gray16:
		return 2
	case *image.NRGBA64, *image.RGBA64:
		return 8
	default:
		return 4
	}
}

func validateImagePix(m image.Image, d image.Point) error {
	switch m := m.(type) {
	case *image.Paletted:
		return validatePix(m.Pix, m.Stride, d.X, d.Y, 1)
	case *image.Gray:
		return validatePix(m.Pix, m.Stride, d.X, d.Y, 1)
	case *image.Gray16:
		return validatePix(m.Pix, m.Stride, d.X, d.Y, 2)
	case *image.NRGBA:
		return validatePix(m.Pix, m.Stride, d.X, d.Y, 4)
	case *image.NRGBA64:
		return validatePix(m.Pix, m.Stride, d.X, d.Y, 8)
	case *image.RGBA:
		return validatePix(m.Pix, m.Stride, d.X, d.Y, 4)
	case *image.RGBA64:
		return validatePix(m.Pix, m.Stride, d.X, d.Y, 8)
	case *image.CMYK:
		return validatePix(m.Pix, m.Stride, d.X, d.Y, 4)
	}
	return nil
}

func validatePix(pix []byte, stride, width, height, bytesPerPixel int) error {
	if width == 0 || height == 0 {
		return nil
	}
	rowLen, ok := checkedMul3(width, bytesPerPixel, 1)
	if !ok || rowLen > int64(maxInt()) || stride < int(rowLen) {
		return errors.New("tiff: invalid image stride")
	}
	lastRow, ok := checkedMul3(height-1, stride, 1)
	if !ok || lastRow > int64(maxInt())-rowLen || int(lastRow+rowLen) > len(pix) {
		return errors.New("tiff: invalid image pixel buffer")
	}
	return nil
}

func imageWriter(compression uint32, buf *bytes.Buffer) (io.Writer, io.Closer, error) {
	switch compression {
	case cNone:
		return buf, nil, nil
	case cLZW:
		w := newTIFFLZWWriter(buf)
		return w, w, nil
	case cDeflate:
		w := zlib.NewWriter(buf)
		return w, w, nil
	}
	return nil, nil, UnsupportedError(fmt.Sprintf("compression value %d", compression))
}

func encodeImage(w io.Writer, m image.Image, d image.Point, predictor bool) (uint32, uint32, []uint32, uint32, []uint32, error) {
	photometricInterpretation := uint32(pRGB)
	samplesPerPixel := uint32(4)
	bitsPerSample := []uint32{8, 8, 8, 8}
	extraSamples := uint32(0)
	colorMap := []uint32{}
	var err error
	switch m := m.(type) {
	case *image.Paletted:
		photometricInterpretation = pPaletted
		samplesPerPixel = 1
		bitsPerSample = []uint32{8}
		colorMap = make([]uint32, 256*3)
		for i := 0; i < 256 && i < len(m.Palette); i++ {
			r, g, b, _ := m.Palette[i].RGBA()
			colorMap[i+0*256] = uint32(r)
			colorMap[i+1*256] = uint32(g)
			colorMap[i+2*256] = uint32(b)
		}
		err = encodeGray(w, m.Pix, d.X, d.Y, m.Stride, predictor)
	case *image.Gray:
		photometricInterpretation = pBlackIsZero
		samplesPerPixel = 1
		bitsPerSample = []uint32{8}
		err = encodeGray(w, m.Pix, d.X, d.Y, m.Stride, predictor)
	case *image.Gray16:
		photometricInterpretation = pBlackIsZero
		samplesPerPixel = 1
		bitsPerSample = []uint32{16}
		err = encodeGray16(w, m.Pix, d.X, d.Y, m.Stride, predictor)
	case *image.NRGBA:
		extraSamples = 2 // Unassociated alpha.
		err = encodeRGBA(w, m.Pix, d.X, d.Y, m.Stride, predictor)
	case *image.NRGBA64:
		extraSamples = 2 // Unassociated alpha.
		bitsPerSample = []uint32{16, 16, 16, 16}
		err = encodeRGBA64(w, m.Pix, d.X, d.Y, m.Stride, predictor)
	case *image.RGBA:
		extraSamples = 1 // Associated alpha.
		err = encodeRGBA(w, m.Pix, d.X, d.Y, m.Stride, predictor)
	case *image.RGBA64:
		extraSamples = 1 // Associated alpha.
		bitsPerSample = []uint32{16, 16, 16, 16}
		err = encodeRGBA64(w, m.Pix, d.X, d.Y, m.Stride, predictor)
	case *image.CMYK:
		photometricInterpretation = uint32(pCMYK)
		samplesPerPixel = uint32(4)
		bitsPerSample = []uint32{8, 8, 8, 8}
		err = encodeCMYK(w, m.Pix, d.X, d.Y, m.Stride, predictor)
	default:
		extraSamples = 1 // Associated alpha.
		err = encode(w, m, predictor)
	}
	return photometricInterpretation, samplesPerPixel, bitsPerSample, extraSamples, colorMap, err
}

func encodePage(m image.Image, opt *Options) (encodedImage, error) {
	compression, predictor := compressionOptions(opt)
	d, err := imageDimensions(m, compression)
	if err != nil {
		return encodedImage{}, err
	}
	var buf bytes.Buffer
	dst, closer, err := imageWriter(compression, &buf)
	if err != nil {
		return encodedImage{}, err
	}

	pr := uint32(prNone)
	if predictor {
		pr = prHorizontal
	}
	photometricInterpretation, samplesPerPixel, bitsPerSample, extraSamples, colorMap, err :=
		encodeImage(dst, m, d, predictor)
	if err != nil {
		return encodedImage{}, err
	}
	if closer != nil {
		if err = closer.Close(); err != nil {
			return encodedImage{}, err
		}
	}
	if uint64(buf.Len()) > math.MaxUint32 {
		return encodedImage{}, errors.New("tiff: encoded image data too large")
	}

	imageLen := buf.Len()
	ifd := []ifdEntry{
		integerIFDEntry(tImageWidth, uint32(d.X)),
		integerIFDEntry(tImageLength, uint32(d.Y)),
		{tBitsPerSample, dtShort, bitsPerSample},
		{tCompression, dtShort, []uint32{compression}},
		{tPhotometricInterpretation, dtShort, []uint32{photometricInterpretation}},
		{tStripOffsets, dtLong, []uint32{0}},
		{tSamplesPerPixel, dtShort, []uint32{samplesPerPixel}},
		integerIFDEntry(tRowsPerStrip, uint32(d.Y)),
		{tStripByteCounts, dtLong, []uint32{uint32(imageLen)}},
		// There is currently no support for storing the image
		// resolution, so give a bogus value of 72x72 dpi.
		{tXResolution, dtRational, []uint32{72, 1}},
		{tYResolution, dtRational, []uint32{72, 1}},
		{tResolutionUnit, dtShort, []uint32{resPerInch}},
	}
	if pr != prNone {
		ifd = append(ifd, ifdEntry{tPredictor, dtShort, []uint32{pr}})
	}
	if len(colorMap) != 0 {
		ifd = append(ifd, ifdEntry{tColorMap, dtShort, colorMap})
	}
	if extraSamples > 0 {
		ifd = append(ifd, ifdEntry{tExtraSamples, dtShort, []uint32{extraSamples}})
	}

	return encodedImage{data: buf.Bytes(), ifd: ifd}, nil
}

func integerIFDEntry(tag int, value uint32) ifdEntry {
	datatype := dtLong
	if value <= math.MaxUint16 {
		datatype = dtShort
	}
	return ifdEntry{tag, datatype, []uint32{value}}
}

func setStripOffset(ifd []ifdEntry, stripOffset uint32) []ifdEntry {
	ifd = append([]ifdEntry(nil), ifd...)
	for i := range ifd {
		if ifd[i].tag == tStripOffsets {
			ifd[i].data = []uint32{stripOffset}
			return ifd
		}
	}
	return append(ifd, ifdEntry{tStripOffsets, dtLong, []uint32{stripOffset}})
}

func checkClassicTIFFOffset(offset uint64) error {
	if offset > 1<<32-1 {
		return errors.New("tiff: file too large for classic TIFF offsets")
	}
	return nil
}

// EncodeAll writes the images in m to w as a multi-page TIFF. opt determines
// the options used for encoding each image, such as the compression type. If
// opt is nil, uncompressed images are written.
func EncodeAll(w io.Writer, m []image.Image, opt *Options) error {
	if len(m) == 0 {
		return errors.New("tiff: no images to encode")
	}
	pages := make([]encodedImage, len(m))
	for i, img := range m {
		if img == nil {
			return errors.New("tiff: nil image")
		}
		page, err := encodePage(img, opt)
		if err != nil {
			return err
		}
		pages[i] = page
	}
	return writePages(w, pages)
}

func writePages(w io.Writer, pages []encodedImage) error {
	if w == nil {
		return errors.New("tiff: nil writer")
	}
	if len(pages) == 0 {
		return errors.New("tiff: no pages to write")
	}
	dataOffsets, ifdOffsets, err := pageOffsets(pages)
	if err != nil {
		return err
	}
	var header [8]byte
	copy(header[:4], leHeader)
	enc.PutUint32(header[4:], ifdOffsets[0])
	if err = writeBytes(w, header[:]); err != nil {
		return err
	}
	for _, page := range pages {
		if err = writeBytes(w, page.data); err != nil {
			return err
		}
	}
	return writePageIFDs(w, pages, dataOffsets, ifdOffsets)
}

func pageOffsets(pages []encodedImage) ([]uint32, []uint32, error) {
	dataOffsets := make([]uint32, len(pages))
	ifdOffsets := make([]uint32, len(pages))
	offset := uint64(8)
	for i, page := range pages {
		if err := checkClassicTIFFOffset(offset); err != nil {
			return nil, nil, err
		}
		dataOffsets[i] = uint32(offset)
		offset += uint64(len(page.data))
	}
	for i, page := range pages {
		if err := checkClassicTIFFOffset(offset); err != nil {
			return nil, nil, err
		}
		ifdOffsets[i] = uint32(offset)
		ifd, err := marshalIFD(uint32(offset), 0, setStripOffset(page.ifd, dataOffsets[i]))
		if err != nil {
			return nil, nil, err
		}
		offset += uint64(len(ifd))
	}
	return dataOffsets, ifdOffsets, checkClassicTIFFOffset(offset)
}

func writePageIFDs(w io.Writer, pages []encodedImage, dataOffsets, ifdOffsets []uint32) error {
	for i, page := range pages {
		nextIFDOffset := uint32(0)
		if i+1 < len(pages) {
			nextIFDOffset = ifdOffsets[i+1]
		}
		ifd, err := marshalIFD(ifdOffsets[i], nextIFDOffset, setStripOffset(page.ifd, dataOffsets[i]))
		if err != nil {
			return err
		}
		if err = writeBytes(w, ifd); err != nil {
			return err
		}
	}
	return nil
}
