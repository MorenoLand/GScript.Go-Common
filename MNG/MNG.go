package mng

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"
)

var tMNGSignature = [...]byte{0x8a, 0x4d, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
var tPNGSignature = [...]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}

type TMNGFrame struct {
	PNG   []byte
	Delay time.Duration
}

type TMNGAnimation struct {
	Width             uint32
	Height            uint32
	TicksPerSecond    uint32
	NominalLayerCount uint32
	NominalFrameCount uint32
	NominalPlayTime   uint32
	SimplicityProfile uint32
	Frames            []TMNGFrame
}

func IsMNG(data []byte) bool {
	return len(data) >= len(tMNGSignature) && bytes.Equal(data[:len(tMNGSignature)], tMNGSignature[:])
}

func tMNGFrameDelay(data []byte) (uint32, bool) {
	if len(data) == 0 {
		return 0, false
	}
	offset := 1
	for offset < len(data) && data[offset] != 0 {
		offset++
	}
	offset++
	if offset >= len(data) {
		return 0, false
	}
	hasDelay := data[offset] != 0
	offset += 4
	if !hasDelay || offset+4 > len(data) {
		return 0, false
	}
	return binary.BigEndian.Uint32(data[offset : offset+4]), true
}

func ParseMNG(data []byte) (*TMNGAnimation, error) {
	if !IsMNG(data) {
		return nil, fmt.Errorf("not an MNG image")
	}
	animation := &TMNGAnimation{TicksPerSecond: 100}
	var frame []byte
	var pendingDelay uint32
	hasPendingDelay := false
	inPNG := false
	for offset := len(tMNGSignature); offset < len(data); {
		if len(data)-offset < 12 {
			return nil, fmt.Errorf("truncated MNG chunk")
		}
		dataLength := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		chunkLength := dataLength + 12
		if chunkLength < 12 || chunkLength > len(data)-offset {
			return nil, fmt.Errorf("invalid MNG chunk length")
		}
		chunkType := string(data[offset+4 : offset+8])
		chunkData := data[offset+8 : offset+8+dataLength]
		switch chunkType {
		case "MHDR":
			if len(chunkData) < 28 {
				return nil, fmt.Errorf("invalid MNG header")
			}
			animation.Width = binary.BigEndian.Uint32(chunkData[0:4])
			animation.Height = binary.BigEndian.Uint32(chunkData[4:8])
			if ticks := binary.BigEndian.Uint32(chunkData[8:12]); ticks > 0 {
				animation.TicksPerSecond = ticks
			}
			animation.NominalLayerCount = binary.BigEndian.Uint32(chunkData[12:16])
			animation.NominalFrameCount = binary.BigEndian.Uint32(chunkData[16:20])
			animation.NominalPlayTime = binary.BigEndian.Uint32(chunkData[20:24])
			animation.SimplicityProfile = binary.BigEndian.Uint32(chunkData[24:28])
		case "FRAM":
			pendingDelay, hasPendingDelay = tMNGFrameDelay(chunkData)
		}
		if !inPNG && chunkType == "IHDR" {
			frame = append(frame[:0], tPNGSignature[:]...)
			inPNG = true
		}
		if inPNG {
			frame = append(frame, data[offset:offset+chunkLength]...)
			if chunkType == "IEND" {
				delay := 100 * time.Millisecond
				if hasPendingDelay {
					milliseconds := (uint64(pendingDelay)*1000 + uint64(animation.TicksPerSecond)/2) / uint64(animation.TicksPerSecond)
					if milliseconds > 0 {
						delay = time.Duration(milliseconds) * time.Millisecond
					}
				}
				animation.Frames = append(animation.Frames, TMNGFrame{PNG: append([]byte(nil), frame...), Delay: delay})
				frame = frame[:0]
				hasPendingDelay = false
				inPNG = false
			}
		}
		offset += chunkLength
	}
	if inPNG || len(animation.Frames) == 0 {
		return nil, fmt.Errorf("MNG contains no complete frames")
	}
	return animation, nil
}

func (animation *TMNGAnimation) FrameIndexAt(elapsed time.Duration) int {
	if animation == nil || len(animation.Frames) == 0 {
		return -1
	}
	if len(animation.Frames) == 1 {
		return 0
	}
	var duration time.Duration
	for _, frame := range animation.Frames {
		duration += frame.Delay
	}
	if duration <= 0 {
		return 0
	}
	elapsed %= duration
	if elapsed < 0 {
		elapsed += duration
	}
	for index, frame := range animation.Frames {
		if elapsed < frame.Delay {
			return index
		}
		elapsed -= frame.Delay
	}
	return len(animation.Frames) - 1
}

func FirstMNGPNGFrame(data []byte) ([]byte, error) {
	animation, err := ParseMNG(data)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), animation.Frames[0].PNG...), nil
}
