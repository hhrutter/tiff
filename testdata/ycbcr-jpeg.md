# YCbCr JPEG fixtures

`ycbcr-444.jpg` and `ycbcr-422.jpg` were generated for decoder tests with ImageMagick from a synthetic 16x16 RGB image.
At pixel `(x,y)`, the source color is `(17*x, 17*y, 9*(x+y) mod 256)`.
Both use JPEG quality 85; sampling factors are `1x1` and `2x1`, respectively.

The tests compare TIFF output with Go's direct decoding of these same JPEG payloads.
The remaining 4:2:0 payloads are generated in the tests with Go's JPEG encoder.
These fixtures contain no external image content or personal data.
