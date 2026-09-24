 # Note

This package is an improved version of [x/image/tiff](https://github.com/golang/image/tree/master/tiff) featuring:

* Read support for CCITT Group3/4 compressed images using [x/image/ccitt](https://github.com/golang/image/tree/master/ccitt)
* Read/write support for LZW compressed images.
* Read/write support for the CMYK color model.
* Read support for JPEG compressed images.
* Read support for multi page TIFF files.

## YCbCr JPEG decoding

YCbCr images are supported with JPEG compression (7), three 8-bit components, and interleaved component storage.
The decoder returns an `image.RGBA`; configuration reads report `color.RGBAModel`.
It reads single or multiple strips and tiles, with complete JPEG streams or shared `JPEGTables`.
Supported JPEG frames are 8-bit sequential Huffman frames with one interleaved scan.

Supported subsampling factors are `[1,1]`, `[2,1]`, and `[2,2]`. Chroma positioning must be centered, except that
positioning is immaterial without subsampling. The color coefficients must equal `[299/1000,587/1000,114/1000]`.
The standard defaults apply when these fields or `PlanarConfiguration` are absent.
`ReferenceBlackWhite` must be present and equal `[0,255,128,255,128,255]`.

Other compression methods for YCbCr, separate component planes, extra components, other color conversion parameters,
and other JPEG processes return an error. Applications do not need to import `image/jpeg` separately.


## Background

Working on [pdfcpu](https://github.com/pdfcpu/pdfcpu) (a PDF processor) created a need for processing TIFF files and LZW compression.

1) CCITT compression for monochrome images was the first need. This is being addressed as part of ongoing work on [x/image/ccitt](https://github.com/golang/image/tree/master/ccitt).

2) TIFF LZW compression uses MSB bit order, 8-bit literal codes, and TIFF's code-width transition behavior.

3) The PDF specification defines a CMYK color space. This is currently not supported at [x/image/tiff](https://github.com/golang/image/tree/master/tiff).

## Goal

An improved version of [x/image/tiff](https://github.com/golang/image/tree/master/tiff) with full read/write support for CCITT, LZW, JPEG compression and the CMYK color model.
