package main

// genicons rasterizes the body-plan mark (the same geometry as tools/genlogo,
// shared via tools/internal/mark) into the raster assets the SVG masters cannot
// be: a multi-size Windows .ico for the executables, PNGs for the docs favicon
// and the GitHub social preview, and the tray status icons (the mark tinted
// green/grey/red).
//
// Pure Go, no external rasterizer: each pixel's coverage is its anti-aliased
// distance to the stroked outline.
//
// The mark is an outline, so its stroke weight is corrected per size rather
// than scaled with it — see mark.StrokeWidth. That correction is the reason
// this tool rasterizes every size separately instead of downsampling one large
// render: a downsample would carry the large size's weight down with it and
// vanish.
//
// Run: go run ./tools/genicons
import (
	"bytes"
	"encoding/binary"
	"fmt"
	"go/format"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"

	"github.com/wslkit/skrog/tools/internal/mark"
)

// brand is the slate mark color, matching the SVG masters.
var brand = color.RGBA{0x2F, 0x3B, 0x45, 0xFF}

// ground is the off-white the social card and the avatar sit on. Shared so the
// two cannot drift into two different whites.
var ground = color.RGBA{0xF7, 0xF6, 0xF3, 0xFF}

// writeSocial renders a 1280x640 GitHub social-preview card: the mark centered
// on the off-white brand ground. Uploaded via the repo's settings, not embedded.
func writeSocial(path string) {
	const w, h, markPx = 1280, 640, 440
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{ground}, image.Point{}, draw.Src)
	m := mark.Raster(markPx, brand)
	ox, oy := (w-markPx)/2, (h-markPx)/2
	draw.Draw(dst, image.Rect(ox, oy, ox+markPx, oy+markPx), m, image.Point{}, draw.Over)
	writePNG(path, dst)
}

// writeAvatar renders the square org/user avatar GitHub shows beside
// "wslkit / skrog", and anywhere else an account is named.
//
// It is NOT skrog-512.png, which is the bare mark on a transparent ground.
// That file is right for a docs page that supplies its own background and
// wrong here: the mark is #2F3B45, GitHub's dark theme is #0d1117, and a
// transparent avatar would put one on the other and very nearly disappear.
// An avatar has no styling hook to fix that with — it is an <img> on whatever
// background the viewer's theme picked.
//
// So it gets the same treatment as the social card: an opaque off-white
// ground, which reads on both themes because it carries its own contrast.
//
// GitHub downscales to 460px and then to ~40px in the repo header, so the
// mark is inset rather than bled to the edge — at header size a full-bleed
// mark loses its outline to the crop.
func writeAvatar(path string) {
	const size, markPx = 512, 360
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{ground}, image.Point{}, draw.Src)

	m := mark.Raster(markPx, brand)

	// Centre the INK, not the raster box. The mark's drawing does not fill its
	// own square evenly -- a hull body plan is wider than it is tall and sits
	// low in the frame -- so centring the box left the glyph 13px low at this
	// size, which reads as a misaligned logo rather than as the shape's own
	// proportions.
	//
	// Measured rather than corrected by a constant, so this stays true if the
	// geometry in tools/internal/mark ever changes.
	b := inkBounds(m)
	ox := (size - b.Dx()) / 2
	oy := (size - b.Dy()) / 2
	draw.Draw(dst,
		image.Rect(ox-b.Min.X, oy-b.Min.Y, ox-b.Min.X+markPx, oy-b.Min.Y+markPx),
		m, image.Point{}, draw.Over)
	writePNG(path, dst)
}

// inkBounds is the tight box around everything non-transparent in img.
func inkBounds(img image.Image) image.Rectangle {
	b := img.Bounds()
	minX, minY, maxX, maxY := b.Max.X, b.Max.Y, b.Min.X-1, b.Min.Y-1
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a > 0 {
				if x < minX {
					minX = x
				}
				if y < minY {
					minY = y
				}
				if x > maxX {
					maxX = x
				}
				if y > maxY {
					maxY = y
				}
			}
		}
	}
	if maxX < minX || maxY < minY {
		return b // nothing drawn; centre the box and let the caller see it
	}
	return image.Rect(minX, minY, maxX+1, maxY+1)
}

func writePNG(path string, img image.Image) {
	f, err := os.Create(path)
	must(err)
	defer f.Close()
	must(png.Encode(f, img))
	fmt.Println("wrote", path)
}

func main() {
	must(os.MkdirAll("assets/icons", 0o755))

	// PNGs for docs / previews, plus a 32px favicon.
	// 128 is the VS Code Marketplace's icon size: the extension copies this
	// file rather than keeping a second, hand-cut mark that can drift from the
	// brand source.
	writePNG("assets/icons/skrog-128.png", mark.Raster(128, brand))
	writePNG("assets/icons/skrog-256.png", mark.Raster(256, brand))
	writePNG("assets/icons/skrog-512.png", mark.Raster(512, brand))
	writePNG("assets/icons/favicon-32.png", mark.Raster(32, brand))
	writeSocial("assets/icons/social-preview.png")
	writeAvatar("assets/icons/avatar-512.png")

	// Windows .ico with the sizes Explorer, the taskbar and the tray use.
	writeICO("assets/icons/skrog.ico", []int{16, 24, 32, 48, 64, 128, 256}, brand)

	// Tray status icons: the mark tinted to the engine state (#76: compose
	// status onto the mark, replacing the old plain dot).
	writeTrayIcons("cmd/skrogtray/icons_windows.go")

	fmt.Println("done")
}

// dibICO packs a 16x16 image as a classic 32bpp DIB icon — the format the tray
// loads most reliably at small sizes (PNG-in-ICO can misrender at 16px). The
// alpha channel carries the anti-aliased edges; the AND mask is left zero.
func dibICO(img *image.RGBA) []byte {
	const n = 16
	var dib bytes.Buffer
	binary.Write(&dib, binary.LittleEndian, uint32(40)) // header size
	binary.Write(&dib, binary.LittleEndian, int32(n))   // width
	binary.Write(&dib, binary.LittleEndian, int32(n*2)) // height (XOR+AND)
	binary.Write(&dib, binary.LittleEndian, uint16(1))  // planes
	binary.Write(&dib, binary.LittleEndian, uint16(32)) // bpp
	binary.Write(&dib, binary.LittleEndian, uint32(0))  // compression
	binary.Write(&dib, binary.LittleEndian, uint32(0))  // image size
	binary.Write(&dib, binary.LittleEndian, [4]int32{}) // ppm + colors
	for y := n - 1; y >= 0; y-- {
		for x := 0; x < n; x++ {
			px := img.RGBAAt(x, y)
			dib.Write([]byte{px.B, px.G, px.R, px.A})
		}
	}
	dib.Write(make([]byte, 4*n)) // AND mask, padded to 4 bytes per row

	var out bytes.Buffer
	binary.Write(&out, binary.LittleEndian, uint16(0))
	binary.Write(&out, binary.LittleEndian, uint16(1))
	binary.Write(&out, binary.LittleEndian, uint16(1))
	out.Write([]byte{n, n, 0, 0})
	binary.Write(&out, binary.LittleEndian, uint16(1))
	binary.Write(&out, binary.LittleEndian, uint16(32))
	binary.Write(&out, binary.LittleEndian, uint32(dib.Len()))
	binary.Write(&out, binary.LittleEndian, uint32(22))
	out.Write(dib.Bytes())
	return out.Bytes()
}

// writeTrayIcons regenerates the tray's green/grey/red status icons as the
// mark tinted to each state, emitted as a Go source file of byte slices.
func writeTrayIcons(path string) {
	icons := []struct {
		name string
		c    color.RGBA
	}{
		{"iconGreen", color.RGBA{0x2E, 0xA0, 0x43, 0xFF}}, // running
		{"iconGrey", color.RGBA{0x8C, 0x94, 0x9E, 0xFF}},  // idle / stopped
		{"iconRed", color.RGBA{0xDA, 0x36, 0x33, 0xFF}},   // down / error
	}
	var b bytes.Buffer
	fmt.Fprint(&b, "//go:build windows\n\npackage main\n\n"+
		"// Generated 16x16 body-plan status icons (green/grey/red). The tray shows\n"+
		"// Skrog mark in the engine's state color. Regenerate with `go run ./tools/genicons`.\n\n")
	for _, e := range icons {
		data := dibICO(mark.Raster(16, e.c))
		fmt.Fprintf(&b, "var %s = []byte{", e.name)
		for i, by := range data {
			if i%16 == 0 {
				b.WriteString("\n\t")
			}
			fmt.Fprintf(&b, "0x%02x, ", by)
		}
		b.WriteString("\n}\n\n")
	}
	src, err := format.Source(b.Bytes())
	must(err)
	must(os.WriteFile(path, src, 0o644))
	fmt.Println("wrote", path)
}

// writeICO packs several PNG-encoded sizes into one .ico. PNG-in-ICO is valid
// since Vista and keeps large sizes small; Explorer picks the size it needs.
func writeICO(path string, sizes []int, c color.RGBA) {
	type entry struct {
		size int
		png  []byte
	}
	var entries []entry
	for _, s := range sizes {
		var buf bytes.Buffer
		must(png.Encode(&buf, mark.Raster(s, c)))
		entries = append(entries, entry{s, buf.Bytes()})
	}

	var out bytes.Buffer
	binary.Write(&out, binary.LittleEndian, uint16(0))            // reserved
	binary.Write(&out, binary.LittleEndian, uint16(1))            // type: icon
	binary.Write(&out, binary.LittleEndian, uint16(len(entries))) // count
	offset := 6 + 16*len(entries)                                 // header + directory
	for _, e := range entries {
		dim := byte(e.size)
		if e.size >= 256 {
			dim = 0 // 0 means 256 in the ICO directory
		}
		out.WriteByte(dim)                                          // width
		out.WriteByte(dim)                                          // height
		out.WriteByte(0)                                            // palette
		out.WriteByte(0)                                            // reserved
		binary.Write(&out, binary.LittleEndian, uint16(1))          // planes
		binary.Write(&out, binary.LittleEndian, uint16(32))         // bpp
		binary.Write(&out, binary.LittleEndian, uint32(len(e.png))) // bytes
		binary.Write(&out, binary.LittleEndian, uint32(offset))     // offset
		offset += len(e.png)
	}
	for _, e := range entries {
		out.Write(e.png)
	}
	must(os.WriteFile(path, out.Bytes(), 0o644))
	fmt.Println("wrote", path, "("+filepath.Base(path)+",", len(sizes), "sizes)")
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
