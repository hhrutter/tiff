// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tiff

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
)

const (
	tPlanarConfiguration = 284
	tYCbCrCoefficients   = 529
	tYCbCrSubSampling    = 530
	tYCbCrPositioning    = 531
	tReferenceBlackWhite = 532
)

type ycbcrConfig struct {
	horizontal uint16
	vertical   uint16
}

// ycbcrShorts reads an exact SHORT field, using defaults only when the field is absent.
func (d *decoder) ycbcrShorts(tag, count int, defaults ...uint16) ([]uint16, error) {
	p, found := d.ifd[tag]
	if !found {
		if len(defaults) == count {
			return defaults, nil
		}
		return nil, FormatError(fmt.Sprintf("missing YCbCr tag %d", tag))
	}
	if d.byteOrder.Uint16(p[2:4]) != dtShort || d.byteOrder.Uint32(p[4:8]) != uint32(count) {
		return nil, FormatError(fmt.Sprintf("invalid YCbCr tag %d type or count", tag))
	}
	values, err := d.ifdUint(p[:], count)
	if err != nil {
		return nil, err
	}
	result := make([]uint16, count)
	for i, v := range values {
		result[i] = uint16(v)
	}
	return result, nil
}

func (d *decoder) ycbcrRationals(tag int, required bool, numerators []uint32, denominator uint32) error {
	p, found := d.ifd[tag]
	if !found {
		if required {
			return FormatError(fmt.Sprintf("missing YCbCr tag %d", tag))
		}
		return nil
	}
	if d.byteOrder.Uint16(p[2:4]) != dtRational || d.byteOrder.Uint32(p[4:8]) != uint32(len(numerators)) {
		return FormatError(fmt.Sprintf("invalid YCbCr tag %d type or count", tag))
	}
	bb, err := safeReadAt(d.r, uint64(8*len(numerators)), int64(d.byteOrder.Uint32(p[8:12])))
	if err != nil {
		return err
	}
	for i, want := range numerators {
		n := d.byteOrder.Uint32(bb[8*i:])
		v := d.byteOrder.Uint32(bb[8*i+4:])
		if v == 0 {
			return FormatError(fmt.Sprintf("zero denominator in YCbCr tag %d", tag))
		}
		if uint64(n)*uint64(denominator) != uint64(v)*uint64(want) {
			return UnsupportedError(fmt.Sprintf("YCbCr tag %d values", tag))
		}
	}
	return nil
}

func (d *decoder) initYCbCr() error {
	if err := d.checkYCbCrDimensions(); err != nil {
		return err
	}
	if err := d.checkYCbCrSamples(); err != nil {
		return err
	}
	if err := d.checkYCbCrStorage(); err != nil {
		return err
	}
	if err := d.checkYCbCrSubSampling(); err != nil {
		return err
	}
	if err := d.ycbcrRationals(tYCbCrCoefficients, false, []uint32{299, 587, 114}, 1000); err != nil {
		return err
	}
	// Some older encoders omit ReferenceBlackWhite. Use the full-range default in that case.
	if err := d.ycbcrRationals(tReferenceBlackWhite, false, []uint32{0, 255, 128, 255, 128, 255}, 1); err != nil {
		return err
	}
	if p, found := d.ifd[tJPEGTables]; found {
		if d.byteOrder.Uint16(p[2:4]) != 7 || d.byteOrder.Uint32(p[4:8]) < 4 {
			return FormatError("invalid JPEGTables type or count")
		}
	}
	d.mode, d.bpp = mRGB, 8
	d.config.ColorModel = color.RGBAModel
	return nil
}

func (d *decoder) checkYCbCrSamples() error {
	if _, err := d.ycbcrShorts(tPhotometricInterpretation, 1); err != nil {
		return err
	}
	samples, err := d.ycbcrShorts(tSamplesPerPixel, 1)
	if err != nil {
		return err
	}
	if samples[0] != 3 {
		if samples[0] == 0 {
			return FormatError("zero YCbCr SamplesPerPixel")
		}
		return UnsupportedError("YCbCr SamplesPerPixel other than 3")
	}
	if _, found := d.ifd[tExtraSamples]; found {
		return UnsupportedError("YCbCr ExtraSamples")
	}
	bits, err := d.ycbcrShorts(tBitsPerSample, 3)
	if err != nil {
		return err
	}
	for _, n := range bits {
		if n == 0 {
			return FormatError("zero YCbCr BitsPerSample")
		}
		if n != 8 {
			return UnsupportedError("YCbCr BitsPerSample other than 8")
		}
	}
	return d.checkYCbCrSampleFormat()
}

func (d *decoder) checkYCbCrSampleFormat() error {
	values, err := d.ycbcrShorts(tSampleFormat, 3, 1, 1, 1)
	if err != nil {
		return err
	}
	for _, v := range values {
		if v != 1 {
			return UnsupportedError("YCbCr sample format")
		}
	}
	return nil
}

func (d *decoder) checkYCbCrStorage() error {
	compression, err := d.ycbcrShorts(tCompression, 1)
	if err != nil {
		return err
	}
	if compression[0] != cJPEG {
		return UnsupportedError(fmt.Sprintf("YCbCr compression %d", compression[0]))
	}
	planar, err := d.ycbcrShorts(tPlanarConfiguration, 1, 1)
	if err != nil {
		return err
	}
	switch planar[0] {
	case 1:
	case 2:
		return UnsupportedError("YCbCr separate component planes")
	default:
		return FormatError("invalid YCbCr PlanarConfiguration")
	}
	predictor, err := d.ycbcrShorts(tPredictor, 1, 1)
	if err != nil {
		return err
	}
	if predictor[0] != 1 {
		return FormatError("predictor with YCbCr JPEG compression")
	}
	return nil
}

func (d *decoder) checkYCbCrSubSampling() error {
	factors, err := d.ycbcrShorts(tYCbCrSubSampling, 2, 2, 2)
	if err != nil {
		return err
	}
	h, v := factors[0], factors[1]
	validFactor := func(n uint16) bool { return n == 1 || n == 2 || n == 4 }
	if !validFactor(h) || !validFactor(v) || v > h {
		return FormatError("invalid YCbCrSubSampling")
	}
	if h > 2 || v > 2 {
		return UnsupportedError("YCbCrSubSampling with factor 4")
	}
	position, err := d.ycbcrShorts(tYCbCrPositioning, 1, 1)
	if err != nil {
		return err
	}
	if position[0] != 1 && position[0] != 2 {
		return FormatError("invalid YCbCrPositioning")
	}
	if position[0] == 2 && (h != 1 || v != 1) {
		return UnsupportedError("cosited YCbCr subsampling")
	}
	d.ycbcr = &ycbcrConfig{horizontal: h, vertical: v}
	return nil
}

type ycbcrLayout struct {
	width, height int
	across, down  int
	tiled         bool
	offsets       ifdValues
	counts        ifdValues
}

func (d *decoder) ycbcrInteger(tag int, required bool) error {
	p, found := d.ifd[tag]
	if !found {
		if required {
			return FormatError(fmt.Sprintf("missing YCbCr tag %d", tag))
		}
		return nil
	}
	typ := d.byteOrder.Uint16(p[2:4])
	if (typ != dtShort && typ != dtLong) || d.byteOrder.Uint32(p[4:8]) != 1 {
		return FormatError(fmt.Sprintf("invalid YCbCr tag %d type or count", tag))
	}
	v, err := d.ifdUint(p[:], 1)
	if err != nil {
		return err
	}
	if v[0] == 0 {
		return FormatError(fmt.Sprintf("zero YCbCr tag %d", tag))
	}
	return nil
}

func (d *decoder) checkYCbCrDimensions() error {
	for _, tag := range []int{tImageWidth, tImageLength} {
		if err := d.ycbcrInteger(tag, true); err != nil {
			return err
		}
	}
	_, width := d.ifd[tTileWidth]
	_, height := d.ifd[tTileLength]
	if width != height {
		return FormatError("incomplete YCbCr tile dimensions")
	}
	for _, tag := range []int{tTileWidth, tTileLength, tRowsPerStrip} {
		if err := d.ycbcrInteger(tag, false); err != nil {
			return err
		}
	}
	return nil
}

func (d *decoder) ycbcrOffsets(tag, count int) (ifdValues, error) {
	p, found := d.ifd[tag]
	if !found {
		return ifdValues{}, FormatError(fmt.Sprintf("missing YCbCr tag %d", tag))
	}
	typ := d.byteOrder.Uint16(p[2:4])
	if (typ != dtShort && typ != dtLong) || uint64(d.byteOrder.Uint32(p[4:8])) != uint64(count) {
		return ifdValues{}, FormatError(fmt.Sprintf("invalid YCbCr tag %d type or count", tag))
	}
	return d.parseIFDOffsets(tag, count)
}

func (d *decoder) ycbcrLayout() (ycbcrLayout, error) {
	var l ycbcrLayout
	if d.config.Width == 0 || d.config.Height == 0 {
		return l, FormatError("empty YCbCr image")
	}
	l.width, l.height, l.across = d.config.Width, d.config.Height, 1
	_, l.tiled = d.ifd[tTileWidth]
	offsetTag, countTag := tStripOffsets, tStripByteCounts
	if l.tiled {
		offsetTag, countTag = tTileOffsets, tTileByteCounts
		if err := d.ycbcrTileLayout(&l); err != nil {
			return l, err
		}
	} else if err := d.ycbcrStripLayout(&l); err != nil {
		return l, err
	}
	l.down = 1 + (d.config.Height-1)/l.height
	if _, ok := checkedMul3(l.width, l.height, maxBytesPerPixel); !ok {
		return l, FormatError("YCbCr segment too large")
	}
	var err error
	l.offsets, err = d.ycbcrOffsets(offsetTag, l.across*l.down)
	if err != nil {
		return l, err
	}
	l.counts, err = d.ycbcrOffsets(countTag, l.across*l.down)
	if err != nil {
		return l, err
	}
	return l, nil
}

func (d *decoder) ycbcrTileLayout(l *ycbcrLayout) error {
	var err error
	if l.width, err = d.firstIntVal(tTileWidth); err != nil {
		return err
	}
	if l.height, err = d.firstIntVal(tTileLength); err != nil {
		return err
	}
	// TIFF tiles are multiples of 16; these also align the supported JPEG MCUs.
	if l.width <= 0 || l.height <= 0 || l.width%16 != 0 || l.height%16 != 0 {
		return FormatError("invalid YCbCr JPEG tile dimensions")
	}
	l.across = 1 + (d.config.Width-1)/l.width
	return nil
}

func (d *decoder) ycbcrStripLayout(l *ycbcrLayout) error {
	rows, err := d.firstIntVal(tRowsPerStrip)
	if err != nil {
		return err
	}
	if rows > 0 && rows < l.height {
		l.height = rows
		// See TIFF Technical Note #2: JPEG strip boundaries must align with MCUs.
		if rows%(8*int(d.ycbcr.vertical)) != 0 {
			return FormatError("YCbCr JPEG RowsPerStrip is not MCU aligned")
		}
	}
	return nil
}

func (d *decoder) decodeYCbCr() (image.Image, error) {
	l, err := d.ycbcrLayout()
	if err != nil {
		return nil, err
	}
	tables, err := d.jpegTables()
	if err != nil {
		return nil, err
	}
	img := image.NewRGBA(image.Rect(0, 0, d.config.Width, d.config.Height))
	for i := 0; i < l.offsets.count; i++ {
		if err := d.decodeYCbCrSegment(img, &l, i, tables); err != nil {
			return nil, err
		}
	}
	return img, nil
}

func (d *decoder) decodeYCbCrSegment(img *image.RGBA, l *ycbcrLayout, index int, tables []byte) error {
	x, y := (index%l.across)*l.width, (index/l.across)*l.height
	height := l.height
	if !l.tiled && height > d.config.Height-y {
		height = d.config.Height - y
	}
	offset, err := l.offsets.value(index)
	if err != nil {
		return err
	}
	n, err := l.counts.value(index)
	if err != nil {
		return err
	}
	r, err := d.ycbcrJPEGReader(int64(offset), int64(n), l.width, height, tables)
	if err != nil {
		return err
	}
	segment, err := jpeg.Decode(r)
	if err != nil {
		return err
	}
	draw.Draw(img, image.Rect(x, y, x+l.width, y+height), segment, segment.Bounds().Min, draw.Src)
	return nil
}

func decodeImage(d *decoder) (image.Image, error) {
	if d.ycbcr != nil {
		return d.decodeYCbCr()
	}
	return decode(d)
}
