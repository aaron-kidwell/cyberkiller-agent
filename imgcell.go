package main

import (
	"bytes"
	"image"
	"image/color"
	_ "image/jpeg" // register JPEG decoder
	_ "image/png"  // register PNG decoder
	"strings"
)

// Color depth the terminal supports, detected from the environment.
type colorMode int

const (
	colorTrue colorMode = iota // 24-bit \033[38;2;r;g;bm
	color256                   // 256-color \033[38;5;Nm
	colorNone                  // no/ambiguous color → caller uses ANSI art
)

// renderImageCells decodes image bytes and renders them as terminal cells using
// the upper-half-block glyph "▀": the cell's FOREGROUND color is the top pixel
// and the BACKGROUND color is the bottom pixel, so each character row shows two
// image rows. Works on ANY terminal with at least 256 colors — truecolor when
// available (Kali qterminal, Parrot mate-terminal, gnome-terminal, kitty, foot,
// iTerm2, Alacritty), 256-color quantized otherwise (older xterm, basic tmux).
// No Sixel/Kitty graphics protocol needed, so it renders for everyone.
//
// Transparent pixels are composited over bg. Returns nil if decode fails or the
// terminal has no usable color (caller falls back to ANSI block art).
func renderImageCells(data []byte, cols int, bg color.RGBA, mode colorMode) []string {
	if mode == colorNone {
		return nil
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}

	b := src.Bounds()
	iw, ih := b.Dx(), b.Dy()
	if iw == 0 || ih == 0 {
		return nil
	}

	// Preserve aspect: terminal cells are ~2x taller than wide, and each cell
	// holds 2 vertical pixels, so rows = ih*cols/iw/2.
	rows := ih * cols / iw / 2
	if rows < 1 {
		rows = 1
	}
	gridH := rows * 2 // sampled pixel rows

	// Sample with alpha so we can keep transparent areas truly blank (the
	// terminal background shows through) rather than painting a dark box.
	at := func(gx, gy int) (color.RGBA, float64) {
		sx := b.Min.X + gx*iw/cols
		sy := b.Min.Y + gy*ih/gridH
		r, g, bl, a := src.At(sx, sy).RGBA() // 16-bit
		af := float64(a) / 65535.0
		// Composite over bg only for the visible glow falloff.
		comp := func(c uint32, bgc uint8) uint8 {
			cv := float64(c) / 65535.0 * 255.0
			return uint8(cv*af + float64(bgc)*(1-af))
		}
		return color.RGBA{comp(r, bg.R), comp(g, bg.G), comp(bl, bg.B), 255}, af
	}
	const aThresh = 0.12 // below this a half-pixel is treated as transparent

	fgSeq := func(c color.RGBA) string {
		if mode == colorTrue {
			return "\033[38;2;" + itoa(int(c.R)) + ";" + itoa(int(c.G)) + ";" + itoa(int(c.B)) + "m"
		}
		return "\033[38;5;" + itoa(rgbTo256(c)) + "m"
	}
	fgbgSeq := func(top, bot color.RGBA) string {
		if mode == colorTrue {
			return "\033[38;2;" + itoa(int(top.R)) + ";" + itoa(int(top.G)) + ";" + itoa(int(top.B)) +
				";48;2;" + itoa(int(bot.R)) + ";" + itoa(int(bot.G)) + ";" + itoa(int(bot.B)) + "m"
		}
		return "\033[38;5;" + itoa(rgbTo256(top)) + ";48;5;" + itoa(rgbTo256(bot)) + "m"
	}

	lines := make([]string, 0, rows)
	for ry := 0; ry < rows; ry++ {
		var sb strings.Builder
		for cx := 0; cx < cols; cx++ {
			top, ta := at(cx, ry*2)
			bot, ba := at(cx, ry*2+1)
			switch {
			case ta < aThresh && ba < aThresh:
				sb.WriteString(resetSGR + " ") // both transparent → terminal bg
			case ta >= aThresh && ba < aThresh:
				sb.WriteString(resetSGR + fgSeq(top) + "▀") // top only
			case ta < aThresh && ba >= aThresh:
				sb.WriteString(resetSGR + fgSeq(bot) + "▄") // bottom only
			default:
				sb.WriteString(fgbgSeq(top, bot) + "▀") // both → full color cell
			}
		}
		sb.WriteString(resetSGR)
		lines = append(lines, sb.String())
	}
	return lines
}

// brailleGraph renders values as a btop-style filled area graph using braille
// dots (2 wide × 4 tall per cell = high resolution). Newest sample is at the
// right edge. The area is color-graduated bottom→top (lowC→highC). `mode`
// controls color depth; unicode braille is used in all color modes, and a
// block fallback is used only when truecolor/256 are both unavailable.
func brailleGraph(values []float64, cols, rows int, peak float64, lowC, highC color.RGBA, mode colorMode) []string {
	// Two data points per character column (left dots + right dots) for btop's
	// 2x horizontal resolution — the top edge traces the data smoothly.
	nData := cols * 2
	data := make([]float64, nData)
	start := len(values) - nData
	for i := 0; i < nData; i++ {
		if start+i >= 0 && start+i < len(values) {
			data[i] = values[start+i]
		}
	}
	// Scale to the MAX of the visible window (with headroom), like btop. Using
	// a decaying global peak made the graph ramp/drop (false sawtooth); this
	// keeps the shape faithful to the actual data.
	peak = 1
	for _, v := range data {
		if v > peak {
			peak = v
		}
	}
	peak *= 1.15 // headroom so the tallest bar doesn't slam the ceiling
	if mode == colorNone {
		return blockGraph(values, cols, rows, peak)
	}
	totalDots := rows * 4
	// dot bit per (column, dotRow) where dotRow 0=bottom .. 3=top of a cell.
	leftbits := [4]int{0x40, 0x04, 0x02, 0x01}
	rightbits := [4]int{0x80, 0x20, 0x10, 0x08}
	levelAt := func(v float64) int { return int(v / peak * float64(totalDots)) }

	lines := make([]string, rows)
	for r := 0; r < rows; r++ { // r=0 is the TOP row
		var sb strings.Builder
		// Vertical gradient: bottom rows = lowC, top rows = highC (btop look).
		frac := 1.0 - float64(r)/float64(maxInt(rows-1, 1))
		rc := lerpColor(lowC, highC, frac)
		if mode == colorTrue {
			sb.WriteString("\033[38;2;")
			writeRGB(&sb, rc)
			sb.WriteByte('m')
		} else {
			sb.WriteString("\033[38;5;")
			sb.WriteString(itoa(rgbTo256(rc)))
			sb.WriteByte('m')
		}
		cellBottom := (rows - 1 - r) * 4 // global dot level at this cell's bottom
		for x := 0; x < cols; x++ {
			lLevel := levelAt(data[2*x])
			rLevel := levelAt(data[2*x+1])
			ch := 0x2800
			for d := 0; d < 4; d++ {
				if cellBottom+d < lLevel {
					ch |= leftbits[d]
				}
				if cellBottom+d < rLevel {
					ch |= rightbits[d]
				}
			}
			sb.WriteRune(rune(ch))
		}
		sb.WriteString(resetSGR)
		lines[r] = sb.String()
	}
	return lines
}

// blockGraph is a no-color/unicode-lite fallback area graph using █ rows.
func blockGraph(values []float64, cols, rows int, peak float64) []string {
	data := make([]float64, cols)
	start := len(values) - cols
	for i := 0; i < cols; i++ {
		if start+i >= 0 && start+i < len(values) {
			data[i] = values[start+i]
		}
	}
	lines := make([]string, rows)
	for r := 0; r < rows; r++ {
		var sb strings.Builder
		for x := 0; x < cols; x++ {
			h := int(data[x] / peak * float64(rows))
			if rows-r <= h {
				sb.WriteByte('#')
			} else {
				sb.WriteByte(' ')
			}
		}
		lines[r] = sb.String()
	}
	return lines
}

func lerpColor(a, b color.RGBA, t float64) color.RGBA {
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	li := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*t) }
	return color.RGBA{li(a.R, b.R), li(a.G, b.G), li(a.B, b.B), 255}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// asciiRamp maps brightness (dark→light) to characters. Pure ASCII so it
// renders on ANY terminal, including ones with no Unicode or color support.
const asciiRamp = " .:-=+*#%@"

// renderImageASCII renders the image as ASCII-density art: each cell's CHARACTER
// is chosen by pixel brightness and (when the terminal supports color) tinted
// with the pixel's actual color. Transparent / near-black pixels become spaces
// so the ghost floats cleanly with no background noise. This is the most
// portable renderer — works on every terminal, colored or not.
func renderImageASCII(data []byte, cols int, bg color.RGBA, mode colorMode) []string {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	b := src.Bounds()
	iw, ih := b.Dx(), b.Dy()
	if iw == 0 || ih == 0 {
		return nil
	}
	// Chars are ~2x taller than wide → halve the row count to keep aspect.
	rows := ih * cols / iw / 2
	if rows < 1 {
		rows = 1
	}
	lines := make([]string, 0, rows)
	for ry := 0; ry < rows; ry++ {
		var sb strings.Builder
		for cx := 0; cx < cols; cx++ {
			sx := b.Min.X + cx*iw/cols
			sy := b.Min.Y + ry*ih/rows
			r16, g16, b16, a16 := src.At(sx, sy).RGBA()
			a := float64(a16) / 65535.0
			r, g, bl := int(r16>>8), int(g16>>8), int(b16>>8)
			// luminance (Rec.601) weighted by alpha
			lum := (0.299*float64(r) + 0.587*float64(g) + 0.114*float64(bl)) * a
			if a < 0.08 || lum < 7 {
				sb.WriteByte(' ') // transparent / near-black → blank
				continue
			}
			idx := int(lum / 256 * float64(len(asciiRamp)))
			if idx >= len(asciiRamp) {
				idx = len(asciiRamp) - 1
			}
			ch := asciiRamp[idx]
			switch mode {
			case colorTrue:
				sb.WriteString("\033[38;2;")
				writeRGB(&sb, color.RGBA{uint8(r), uint8(g), uint8(bl), 255})
				sb.WriteByte('m')
				sb.WriteByte(ch)
			case color256:
				sb.WriteString("\033[38;5;")
				sb.WriteString(itoa(rgbTo256(color.RGBA{uint8(r), uint8(g), uint8(bl), 255})))
				sb.WriteByte('m')
				sb.WriteByte(ch)
			default: // colorNone — plain ASCII, still shows the shape
				sb.WriteByte(ch)
			}
		}
		if mode != colorNone {
			sb.WriteString(resetSGR)
		}
		lines = append(lines, sb.String())
	}
	return lines
}

func writeRGB(sb *strings.Builder, c color.RGBA) {
	sb.WriteString(itoa(int(c.R)))
	sb.WriteByte(';')
	sb.WriteString(itoa(int(c.G)))
	sb.WriteByte(';')
	sb.WriteString(itoa(int(c.B)))
}

// rgbTo256 maps a 24-bit color to the nearest xterm-256 palette index
// (16-231 color cube + 232-255 grayscale ramp).
func rgbTo256(c color.RGBA) int {
	r, g, b := int(c.R), int(c.G), int(c.B)
	// Near-gray → use the grayscale ramp for smoother result.
	if absdiff(r, g) < 10 && absdiff(g, b) < 10 && absdiff(r, b) < 10 {
		if r < 8 {
			return 16
		}
		if r > 248 {
			return 231
		}
		return 232 + (r-8)*24/247
	}
	cube := func(v int) int {
		if v < 48 {
			return 0
		}
		if v < 115 {
			return 1
		}
		return (v - 35) / 40
	}
	return 16 + 36*cube(r) + 6*cube(g) + cube(b)
}

func absdiff(a, b int) int {
	if a > b {
		return a - b
	}
	return b - a
}

// itoa avoids importing strconv just for this hot path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [3]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
