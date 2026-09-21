package blp

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
)

const (
	blpEncodingJPEG   = 0
	blpEncodingAlpha  = 1
	blpEncodingDXT    = 2
	blpEncodingUncomp = 3
	blpHeaderSize     = 0x494
)

func DecodeBLP(data []byte) (image.Image, error) {
	if len(data) < 148 {
		return nil, fmt.Errorf("blp: truncated header (%d bytes)", len(data))
	}
	if string(data[0:4]) != "BLP2" {
		return nil, fmt.Errorf("blp: bad magic %q", data[0:4])
	}
	encoding, alphaDepth, alphaType := data[8], uint32(data[9]), data[10]
	width, height := int(binary.LittleEndian.Uint32(data[12:16])), int(binary.LittleEndian.Uint32(data[16:20]))
	if width <= 0 || height <= 0 || width > 4096 || height > 4096 {
		return nil, fmt.Errorf("blp: bad dimensions %dx%d", width, height)
	}
	mipOffset, mipSize := int(binary.LittleEndian.Uint32(data[20:24])), int(binary.LittleEndian.Uint32(data[84:88]))
	if mipOffset == 0 || mipSize <= 0 || mipOffset+mipSize > len(data) {
		return nil, fmt.Errorf("blp: mip 0 out of range (offset %d size %d, file %d)", mipOffset, mipSize, len(data))
	}
	mip := data[mipOffset : mipOffset+mipSize]
	switch encoding {
	case blpEncodingAlpha:
		if len(data) < blpHeaderSize {
			return nil, fmt.Errorf("blp: palette out of range")
		}
		var palette [256]color.RGBA
		for index := range palette {
			palette[index] = color.RGBA{R: data[148+index*4+2], G: data[148+index*4+1], B: data[148+index*4], A: 255}
		}
		return decodePalette(mip, width, height, alphaDepth, palette)
	case blpEncodingDXT:
		return decodeDXT(mip, width, height, alphaDepth, alphaType)
	case blpEncodingUncomp:
		return decodeRaw(mip, width, height)
	case blpEncodingJPEG:
		return decodeJPEG(data, mip, width, height)
	default:
		return nil, fmt.Errorf("blp: unknown encoding %d", encoding)
	}
}

func decodePalette(mip []byte, width, height int, alphaDepth uint32, palette [256]color.RGBA) (image.Image, error) {
	pixels := mip
	if len(pixels) < width*height {
		return nil, fmt.Errorf("blp: palette pixel data short (%d < %d)", len(pixels), width*height)
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	alpha := pixels[width*height:]
	if alphaDepth == 8 && len(alpha) < width*height || alphaDepth == 4 && len(alpha) < (width*height+1)/2 || alphaDepth == 1 && len(alpha) < (width*height+7)/8 {
		return nil, fmt.Errorf("blp: alpha data short")
	}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			c := palette[pixels[y*width+x]]
			index := y*width + x
			switch alphaDepth {
			case 8:
				c.A = alpha[index]
			case 4:
				nibble := alpha[index/2]
				if index&1 != 0 {
					nibble >>= 4
				} else {
					nibble &= 0x0f
				}
				c.A = nibble | nibble<<4
			case 1:
				if alpha[index/8]>>(uint(index)&7)&1 == 0 {
					c.A = 0
				}
			case 0:
			default:
				return nil, fmt.Errorf("blp: unsupported palette alpha depth %d", alphaDepth)
			}
			img.Set(x, y, color.NRGBA{R: c.R, G: c.G, B: c.B, A: c.A})
		}
	}
	return img, nil
}

func decodeDXT(mip []byte, width, height int, alphaDepth uint32, alphaType uint8) (image.Image, error) {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	blockSize := 8
	if alphaType != 0 {
		blockSize = 16
	}
	need := ((width + 3) / 4) * ((height + 3) / 4) * blockSize
	if len(mip) < need {
		return nil, fmt.Errorf("blp: dxt data short (%d < %d)", len(mip), need)
	}
	for by := 0; by < height; by += 4 {
		for bx := 0; bx < width; bx += 4 {
			offset := ((bx / 4) + (by/4)*((width+3)/4)) * blockSize
			switch alphaType {
			case 0:
				decodeDXT1Block(mip[offset:offset+8], img, bx, by, alphaDepth == 1)
			case 1:
				decodeDXT3Block(mip[offset:offset+16], img, bx, by)
			case 7:
				decodeDXT5Block(mip[offset:offset+16], img, bx, by)
			default:
				return nil, fmt.Errorf("blp: unsupported DXT alpha type %d", alphaType)
			}
		}
	}
	return img, nil
}

func decodeRaw(mip []byte, width, height int) (image.Image, error) {
	need := width * height * 4
	if len(mip) < need {
		return nil, fmt.Errorf("blp: raw data short (%d < %d)", len(mip), need)
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			offset := (y*width + x) * 4
			img.SetNRGBA(x, y, color.NRGBA{R: mip[offset+2], G: mip[offset+1], B: mip[offset], A: mip[offset+3]})
		}
	}
	return img, nil
}

func decodeJPEG(data, mip []byte, width, height int) (image.Image, error) {
	header := []byte{}
	if len(data) >= 152 {
		size := int(binary.LittleEndian.Uint32(data[148:152]))
		if size > 0 && 152+size <= len(data) {
			header = data[152 : 152+size]
		}
	}
	jpegData := append(append([]byte(nil), header...), mip...)
	decoded, err := jpeg.Decode(bytes.NewReader(jpegData))
	if err != nil {
		return nil, fmt.Errorf("blp: jpeg decode: %w", err)
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), decoded, image.Point{}, draw.Src)
	return img, nil
}

func decodeDXT1Block(block []byte, img *image.NRGBA, bx, by int, hasAlpha bool) {
	c0, c1 := binary.LittleEndian.Uint16(block[0:2]), binary.LittleEndian.Uint16(block[2:4])
	r0, g0, b0 := rgb565(c0)
	r1, g1, b1 := rgb565(c1)
	var palette [4][3]uint8
	palette[0], palette[1] = [3]uint8{r0, g0, b0}, [3]uint8{r1, g1, b1}
	if c0 > c1 {
		palette[2] = [3]uint8{uint8((2*uint16(r0) + uint16(r1)) / 3), uint8((2*uint16(g0) + uint16(g1)) / 3), uint8((2*uint16(b0) + uint16(b1)) / 3)}
		palette[3] = [3]uint8{uint8((uint16(r0) + 2*uint16(r1)) / 3), uint8((uint16(g0) + 2*uint16(g1)) / 3), uint8((uint16(b0) + 2*uint16(b1)) / 3)}
	} else {
		palette[2] = [3]uint8{uint8((uint16(r0) + uint16(r1)) / 2), uint8((uint16(g0) + uint16(g1)) / 2), uint8((uint16(b0) + uint16(b1)) / 2)}
	}
	bits := binary.LittleEndian.Uint32(block[4:8])
	for py := 0; py < 4; py++ {
		for px := 0; px < 4; px++ {
			x, y := bx+px, by+py
			if x >= img.Bounds().Dx() || y >= img.Bounds().Dy() {
				continue
			}
			code := uint8(bits >> (2 * uint(py*4+px)) & 3)
			alpha := uint8(255)
			if hasAlpha && c0 <= c1 && code == 3 {
				alpha = 0
			}
			img.SetNRGBA(x, y, color.NRGBA{R: palette[code][0], G: palette[code][1], B: palette[code][2], A: alpha})
		}
	}
}

func decodeDXT3Block(block []byte, img *image.NRGBA, bx, by int) {
	c0, c1 := binary.LittleEndian.Uint16(block[8:10]), binary.LittleEndian.Uint16(block[10:12])
	r0, g0, b0 := rgb565(c0)
	r1, g1, b1 := rgb565(c1)
	palette := [4][3]uint8{{r0, g0, b0}, {r1, g1, b1}, {uint8((2*uint16(r0) + uint16(r1)) / 3), uint8((2*uint16(g0) + uint16(g1)) / 3), uint8((2*uint16(b0) + uint16(b1)) / 3)}, {uint8((uint16(r0) + 2*uint16(r1)) / 3), uint8((uint16(g0) + 2*uint16(g1)) / 3), uint8((uint16(b0) + 2*uint16(b1)) / 3)}}
	bits := binary.LittleEndian.Uint32(block[12:16])
	for py := 0; py < 4; py++ {
		for px := 0; px < 4; px++ {
			x, y := bx+px, by+py
			if x >= img.Bounds().Dx() || y >= img.Bounds().Dy() {
				continue
			}
			code := uint8(bits >> (2 * uint(py*4+px)) & 3)
			index := py*4 + px
			alpha := block[index/2]
			if index&1 == 0 {
				alpha &= 0x0f
			} else {
				alpha >>= 4
			}
			img.SetNRGBA(x, y, color.NRGBA{R: palette[code][0], G: palette[code][1], B: palette[code][2], A: alpha | alpha<<4})
		}
	}
}

func decodeDXT5Block(block []byte, img *image.NRGBA, bx, by int) {
	a0, a1 := block[0], block[1]
	var alpha [8]uint8
	alpha[0], alpha[1] = a0, a1
	if a0 > a1 {
		for i := 0; i < 6; i++ {
			alpha[2+i] = uint8((uint16(6-i)*uint16(a0) + uint16(i+1)*uint16(a1)) / 7)
		}
	} else {
		for i := 0; i < 4; i++ {
			alpha[2+i] = uint8((uint16(4-i)*uint16(a0) + uint16(i+1)*uint16(a1)) / 5)
		}
		alpha[6], alpha[7] = 0, 255
	}
	var alphaBits uint64
	for i := 0; i < 6; i++ {
		alphaBits |= uint64(block[2+i]) << (8 * uint(i))
	}
	c0, c1 := binary.LittleEndian.Uint16(block[8:10]), binary.LittleEndian.Uint16(block[10:12])
	r0, g0, b0 := rgb565(c0)
	r1, g1, b1 := rgb565(c1)
	palette := [4][3]uint8{{r0, g0, b0}, {r1, g1, b1}, {uint8((2*uint16(r0) + uint16(r1)) / 3), uint8((2*uint16(g0) + uint16(g1)) / 3), uint8((2*uint16(b0) + uint16(b1)) / 3)}, {uint8((uint16(r0) + 2*uint16(r1)) / 3), uint8((uint16(g0) + 2*uint16(g1)) / 3), uint8((uint16(b0) + 2*uint16(b1)) / 3)}}
	bits := binary.LittleEndian.Uint32(block[12:16])
	for py := 0; py < 4; py++ {
		for px := 0; px < 4; px++ {
			x, y := bx+px, by+py
			if x >= img.Bounds().Dx() || y >= img.Bounds().Dy() {
				continue
			}
			code := uint8(bits >> (2 * uint(py*4+px)) & 3)
			aIndex := uint8(alphaBits >> (3 * uint(py*4+px)) & 7)
			img.SetNRGBA(x, y, color.NRGBA{R: palette[code][0], G: palette[code][1], B: palette[code][2], A: alpha[aIndex]})
		}
	}
}

func rgb565(value uint16) (uint8, uint8, uint8) {
	return uint8(float64((value>>11)&0x1f) / 31 * 255), uint8(float64((value>>5)&0x3f) / 63 * 255), uint8(float64(value&0x1f) / 31 * 255)
}
