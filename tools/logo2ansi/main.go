// Command logo2ansi converts the logo JPEG into half-block terminal art.
//
// It crops the near-white background, box-downsamples the image, and writes
// four files: truecolor (.ans) and plain (.txt) variants of a full splash and a
// compact mini mark, plus logo_web.png (transparent background) for the web view. Background pixels are emitted as plain spaces with no
// colour codes so the art sits on whatever the terminal background is.
//
// Run via `go generate ./assets`.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	imgcolor "image/color"
	_ "image/jpeg"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// px is one downsampled pixel; ok is false for transparent (background).
type px struct {
	r, g, b uint8
	ok      bool
}

func main() {
	in := flag.String("in", "logo.jfif", "source image")
	out := flag.String("out", ".", "output directory")
	fullCols := flag.Int("full", 40, "splash width in columns")
	miniRows := flag.Int("mini", 3, "mini mark height in rows")
	webRows := flag.Int("web", 64, "web logo height in pixels")
	flag.Parse()

	f, err := os.Open(*in)
	if err != nil {
		log.Fatal(err)
	}
	img, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		log.Fatal(err)
	}
	box := crop(img)

	// Each text row holds two pixels (▀: foreground = top, background =
	// bottom), so pixels stay roughly square on a typical 1:2 terminal cell.
	aspect := float64(box.Dx()) / float64(box.Dy())
	fullRows := int(float64(*fullCols)/aspect/2 + 0.5)
	miniCols := max(1, int(float64(*miniRows*2)*aspect+0.5))
	// The mini mark uses a lower coverage threshold so thin legs survive.
	for _, v := range []struct {
		name       string
		cols, rows int
		cover      float64
	}{
		{"full", *fullCols, fullRows, 0.5},
		{"mini", miniCols, *miniRows, 0.3},
	} {
		grid := sample(img, box, v.cols, v.rows*2, v.cover)
		write(filepath.Join(*out, "logo_"+v.name+".ans"), color(grid))
		write(filepath.Join(*out, "logo_"+v.name+".txt"), plain(grid))
	}
	webCols := max(1, int(float64(*webRows)*aspect+0.5))
	write(filepath.Join(*out, "logo_web.png"), pngOf(sample(img, box, webCols, *webRows, 0.5)))
}

func write(path string, data []byte) {
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Fatal(err)
	}
}

func isBackground(r, g, b uint32) bool {
	// 16-bit channels; near-white with low saturation (tolerates JPEG noise).
	lo, hi := min(r, g, b), max(r, g, b)
	return lo > 0xD800 && hi-lo < 0x1800
}

// crop returns the bounding box of non-background pixels.
func crop(img image.Image) image.Rectangle {
	b := img.Bounds()
	box := image.Rectangle{Min: b.Max, Max: b.Min}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if r, g, bl, _ := img.At(x, y).RGBA(); !isBackground(r, g, bl) {
				box = box.Union(image.Rect(x, y, x+1, y+1))
			}
		}
	}
	if box.Empty() {
		log.Fatal("image is entirely background")
	}
	return box
}

// sample box-averages box into a cols x rows pixel grid (rows must be even).
// A cell is opaque when at least cover of its source pixels are foreground; its
// colour is the average of the foreground pixels only, so edges don't bleed
// toward white.
func sample(img image.Image, box image.Rectangle, cols, rows int, cover float64) [][]px {
	grid := make([][]px, rows)
	for gy := range grid {
		grid[gy] = make([]px, cols)
		y0 := box.Min.Y + gy*box.Dy()/rows
		y1 := max(y0+1, box.Min.Y+(gy+1)*box.Dy()/rows)
		for gx := range grid[gy] {
			x0 := box.Min.X + gx*box.Dx()/cols
			x1 := max(x0+1, box.Min.X+(gx+1)*box.Dx()/cols)
			var sr, sg, sb, n, total uint64
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					total++
					r, g, b, _ := img.At(x, y).RGBA()
					if isBackground(r, g, b) {
						continue
					}
					sr, sg, sb, n = sr+uint64(r), sg+uint64(g), sb+uint64(b), n+1
				}
			}
			if n > 0 && float64(n) >= cover*float64(total) {
				grid[gy][gx] = px{uint8(sr / n >> 8), uint8(sg / n >> 8), uint8(sb / n >> 8), true}
			}
		}
	}
	return grid
}

func color(grid [][]px) []byte {
	var buf bytes.Buffer
	for y := 0; y < len(grid); y += 2 {
		end := 0 // drop trailing transparent cells
		for x := range grid[y] {
			if grid[y][x].ok || grid[y+1][x].ok {
				end = x + 1
			}
		}
		styled := false
		for x := range end {
			top, bot := grid[y][x], grid[y+1][x]
			switch {
			case top.ok && bot.ok:
				fmt.Fprintf(&buf, "\x1b[38;2;%d;%d;%d;48;2;%d;%d;%dm▀", top.r, top.g, top.b, bot.r, bot.g, bot.b)
			case top.ok:
				fmt.Fprintf(&buf, "\x1b[38;2;%d;%d;%d;49m▀", top.r, top.g, top.b)
			case bot.ok:
				fmt.Fprintf(&buf, "\x1b[38;2;%d;%d;%d;49m▄", bot.r, bot.g, bot.b)
			default:
				if styled {
					buf.WriteString("\x1b[0m")
				}
				buf.WriteByte(' ')
				styled = false
				continue
			}
			styled = true
		}
		if styled {
			buf.WriteString("\x1b[0m")
		}
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

func plain(grid [][]px) []byte {
	var buf bytes.Buffer
	for y := 0; y < len(grid); y += 2 {
		line := make([]rune, len(grid[y]))
		for x := range grid[y] {
			switch top, bot := grid[y][x].ok, grid[y+1][x].ok; {
			case top && bot:
				line[x] = '█'
			case top:
				line[x] = '▀'
			case bot:
				line[x] = '▄'
			default:
				line[x] = ' '
			}
		}
		buf.WriteString(strings.TrimRight(string(line), " "))
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

func pngOf(grid [][]px) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, len(grid[0]), len(grid)))
	for y, row := range grid {
		for x, p := range row {
			if p.ok {
				img.SetNRGBA(x, y, imgcolor.NRGBA{p.r, p.g, p.b, 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		log.Fatal(err)
	}
	return buf.Bytes()
}
