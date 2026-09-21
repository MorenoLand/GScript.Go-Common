package blp

import (
	"encoding/binary"
	"image/color"
	"testing"
)

func blpFixture(encoding, alphaDepth, alphaType byte, width, height int, payload []byte) []byte {
	data := make([]byte, blpHeaderSize+len(payload))
	copy(data[:4], "BLP2")
	binary.LittleEndian.PutUint32(data[4:8], 1)
	data[8], data[9], data[10] = encoding, alphaDepth, alphaType
	binary.LittleEndian.PutUint32(data[12:16], uint32(width))
	binary.LittleEndian.PutUint32(data[16:20], uint32(height))
	binary.LittleEndian.PutUint32(data[20:24], uint32(blpHeaderSize))
	binary.LittleEndian.PutUint32(data[84:88], uint32(len(payload)))
	copy(data[blpHeaderSize:], payload)
	return data
}

func TestDecodeBLPRawSwapsBGRA(t *testing.T) {
	img, err := DecodeBLP(blpFixture(blpEncodingUncomp, 8, 0, 1, 1, []byte{3, 2, 1, 4}))
	if err != nil {
		t.Fatal(err)
	}
	if got := color.NRGBAModel.Convert(img.At(0, 0)); got != (color.NRGBA{R: 1, G: 2, B: 3, A: 4}) {
		t.Fatalf("raw pixel=%v", got)
	}
}
