package ui

import (
	"bytes"
	"fmt"
	"image"
)

// encodeSixel turns an image into a sixel payload (the part between
// "DCS ... q" and ST). Colours are snapped to a 6x6x6 cube, which is
// plenty for plots. Hand-rolled to avoid pulling in another module.
func encodeSixel(img *image.RGBA) []byte {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	idx := make([]uint8, w*h)
	used := [216]bool{}
	for y := range h {
		for x := range w {
			o := img.PixOffset(b.Min.X+x, b.Min.Y+y)
			r, g, bl := img.Pix[o], img.Pix[o+1], img.Pix[o+2]
			c := uint8(cube(r)*36 + cube(g)*6 + cube(bl))
			idx[y*w+x] = c
			used[c] = true
		}
	}

	var out bytes.Buffer
	// raster attributes: 1:1 pixel aspect, exact size
	fmt.Fprintf(&out, "\"1;1;%d;%d", w, h)
	for c := range 216 {
		if used[c] {
			// sixel colours are percentages
			fmt.Fprintf(&out, "#%d;2;%d;%d;%d", c, c/36*20, c/6%6*20, c%6*20)
		}
	}
	row := make([]byte, w)
	for band := 0; band < h; band += 6 {
		if band > 0 {
			out.WriteByte('-') // next band; a trailing one makes some decoders expect more
		}
		first := true
		for c := range 216 {
			if !used[c] {
				continue
			}
			any := false
			for x := range w {
				var bits byte
				for k := range 6 {
					if y := band + k; y < h && idx[y*w+x] == uint8(c) {
						bits |= 1 << k
					}
				}
				row[x] = 63 + bits
				any = any || bits != 0
			}
			if !any {
				continue
			}
			if !first {
				out.WriteByte('$') // back to the start of this band
			}
			first = false
			fmt.Fprintf(&out, "#%d", c)
			writeRuns(&out, row)
		}
	}
	return out.Bytes()
}

func cube(v uint8) int { return (int(v)*5 + 127) / 255 }

// writeRuns run-length encodes a row of sixel characters.
func writeRuns(out *bytes.Buffer, row []byte) {
	for i := 0; i < len(row); {
		j := i
		for j < len(row) && row[j] == row[i] {
			j++
		}
		if n := j - i; n > 3 {
			fmt.Fprintf(out, "!%d%c", n, row[i])
		} else {
			for range n {
				out.WriteByte(row[i])
			}
		}
		i = j
	}
}
