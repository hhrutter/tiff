 # Note

This package is an improved version of [x/image/tiff](https://github.com/golang/image/tree/master/tiff) featuring:

* Read support for CCITT Group3/4 compressed images using [x/image/ccitt](https://github.com/golang/image/tree/master/ccitt)
* Read/write support for LZW compressed images.
* Read/write support for the CMYK color model.
* Read support for JPEG compressed images.
* Read support for multi page TIFF files.


## Background

Working on [pdfcpu](https://github.com/pdfcpu/pdfcpu) (a PDF processor) created a need for processing TIFF files and LZW compression.

1) CCITT compression for monochrome images was the first need. This is being addressed as part of ongoing work on [x/image/ccitt](https://github.com/golang/image/tree/master/ccitt).

2) TIFF LZW compression uses MSB bit order, 8-bit literal codes, and TIFF's code-width transition behavior.

3) The PDF specification defines a CMYK color space. This is currently not supported at [x/image/tiff](https://github.com/golang/image/tree/master/tiff).

## Goal

An improved version of [x/image/tiff](https://github.com/golang/image/tree/master/tiff) with full read/write support for CCITT, LZW, JPEG compression and the CMYK color model.
