//go:build windows

package tray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"sync"
)

var iconOnce = sync.OnceValue(buildIcon)

// icon returns a 32x32 PNG-in-ICO tray icon built at runtime so the repository
// carries no binary assets.
func icon() []byte {
	return iconOnce()
}

func buildIcon() []byte {
	const size = 32
	accent := color.NRGBA{R: 0x08, G: 0x7F, B: 0x74, A: 0xFF}
	ring := color.NRGBA{R: 0xDF, G: 0xF3, B: 0xEF, A: 0xFF}
	canvas := image.NewNRGBA(image.Rect(0, 0, size, size))
	center := float64(size-1) / 2
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			distance := math.Hypot(float64(x)-center, float64(y)-center)
			switch {
			case distance <= 6:
				canvas.SetNRGBA(x, y, ring)
			case distance <= 9:
				canvas.SetNRGBA(x, y, accent)
			case distance <= 12:
				canvas.SetNRGBA(x, y, ring)
			case distance <= 15:
				canvas.SetNRGBA(x, y, accent)
			}
		}
	}

	pixels := bytes.Buffer{}
	if err := png.Encode(&pixels, canvas); err != nil {
		return nil
	}
	payload := pixels.Bytes()

	container := bytes.Buffer{}
	binary.Write(&container, binary.LittleEndian, uint16(0))
	binary.Write(&container, binary.LittleEndian, uint16(1))
	binary.Write(&container, binary.LittleEndian, uint16(1))
	container.WriteByte(size)
	container.WriteByte(size)
	container.WriteByte(0)
	container.WriteByte(0)
	binary.Write(&container, binary.LittleEndian, uint16(1))
	binary.Write(&container, binary.LittleEndian, uint16(32))
	binary.Write(&container, binary.LittleEndian, uint32(len(payload)))
	binary.Write(&container, binary.LittleEndian, uint32(22))
	container.Write(payload)
	return container.Bytes()
}
