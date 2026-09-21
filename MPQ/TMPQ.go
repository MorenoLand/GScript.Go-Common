package mpq

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
)

const (
	mpqHashTableOffset = 0
	mpqHashNameA       = 1
	mpqHashNameB       = 2
	mpqHashFileKey     = 3
	mpqHashEntryFree   = 0xffffffff
	mpqFileCompress    = 0x00000200
	mpqFileEncrypted   = 0x00010000
	mpqFileFixKey      = 0x00020000
	mpqFileSingleUnit  = 0x01000000
	mpqFileExists      = 0x80000000
	mpqCompressionZlib = 0x02
)

type MPQEntry struct {
	Name           string
	Size           uint32
	CompressedSize uint32
	BlockIndex     uint32
}

type mpqHash struct {
	hashA      uint32
	hashB      uint32
	locale     uint16
	platform   uint16
	blockIndex uint32
}

type mpqBlock struct {
	offset           uint32
	compressedSize   uint32
	uncompressedSize uint32
	flags            uint32
}

type MPQArchive struct {
	name       string
	data       []byte
	sectorSize uint32
	hashes     []mpqHash
	blocks     []mpqBlock
	entries    []MPQEntry
	byName     map[string]MPQEntry
}

var mpqCryptTable [0x500]uint32
var mpqCryptTableOnce sync.Once

func OpenMPQ(name string) (*MPQArchive, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	return NewMPQArchive(name, data)
}

func NewMPQArchive(name string, data []byte) (*MPQArchive, error) {
	if len(data) < 32 || string(data[:4]) != "MPQ\x1a" {
		return nil, fmt.Errorf("%q is not an MPQ archive", name)
	}
	headerSize := binary.LittleEndian.Uint32(data[4:8])
	archiveSize := binary.LittleEndian.Uint32(data[8:12])
	sectorShift := binary.LittleEndian.Uint16(data[14:16])
	hashOffset := binary.LittleEndian.Uint32(data[16:20])
	blockOffset := binary.LittleEndian.Uint32(data[20:24])
	hashCount := binary.LittleEndian.Uint32(data[24:28])
	blockCount := binary.LittleEndian.Uint32(data[28:32])
	if headerSize < 32 || uint64(headerSize) > uint64(len(data)) || archiveSize > uint32(len(data)) || hashCount == 0 || hashCount&(hashCount-1) != 0 {
		return nil, fmt.Errorf("%q has an invalid MPQ header", name)
	}
	hashEnd := uint64(hashOffset) + uint64(hashCount)*16
	blockEnd := uint64(blockOffset) + uint64(blockCount)*16
	if hashOffset < headerSize || blockOffset < hashOffset || hashEnd > uint64(len(data)) || blockEnd > uint64(len(data)) || blockOffset < uint32(hashEnd) {
		return nil, fmt.Errorf("%q has invalid MPQ tables", name)
	}
	mpqCryptTableOnce.Do(mpqInitializeCryptTable)
	hashData := mpqDecrypt(data[hashOffset:hashEnd], 0xc3af3770)
	blockData := mpqDecrypt(data[blockOffset:blockEnd], 0xec83b3a3)
	archive := &MPQArchive{name: name, data: append([]byte(nil), data...), sectorSize: 512 << sectorShift, hashes: make([]mpqHash, hashCount), blocks: make([]mpqBlock, blockCount), byName: make(map[string]MPQEntry)}
	if archive.sectorSize == 0 || archive.sectorSize > 1<<20 {
		return nil, fmt.Errorf("%q has an invalid MPQ sector size", name)
	}
	for index := range archive.hashes {
		offset := index * 16
		archive.hashes[index] = mpqHash{hashA: binary.LittleEndian.Uint32(hashData[offset:]), hashB: binary.LittleEndian.Uint32(hashData[offset+4:]), locale: binary.LittleEndian.Uint16(hashData[offset+8:]), platform: binary.LittleEndian.Uint16(hashData[offset+10:]), blockIndex: binary.LittleEndian.Uint32(hashData[offset+12:])}
	}
	for index := range archive.blocks {
		offset := index * 16
		archive.blocks[index] = mpqBlock{offset: binary.LittleEndian.Uint32(blockData[offset:]), compressedSize: binary.LittleEndian.Uint32(blockData[offset+4:]), uncompressedSize: binary.LittleEndian.Uint32(blockData[offset+8:]), flags: binary.LittleEndian.Uint32(blockData[offset+12:])}
	}
	if err := archive.loadListFile(); err != nil {
		return nil, err
	}
	return archive, nil
}

func (archive *MPQArchive) Name() string {
	if archive == nil {
		return ""
	}
	return archive.name
}

func (archive *MPQArchive) Entries() []MPQEntry {
	if archive == nil {
		return nil
	}
	return append([]MPQEntry(nil), archive.entries...)
}

func (archive *MPQArchive) ReadFile(name string) ([]byte, error) {
	if archive == nil {
		return nil, fmt.Errorf("MPQ archive is unavailable")
	}
	entry, ok := archive.byName[mpqNameKey(name)]
	if !ok {
		blockIndex, found := archive.findBlockIndex(name)
		if !found {
			return nil, fmt.Errorf("MPQ entry %q was not found", name)
		}
		if blockIndex >= uint32(len(archive.blocks)) {
			return nil, fmt.Errorf("MPQ entry %q has an invalid block index", name)
		}
		block := archive.blocks[blockIndex]
		entry = MPQEntry{Name: strings.ReplaceAll(name, "\\", "/"), Size: block.uncompressedSize, CompressedSize: block.compressedSize, BlockIndex: blockIndex}
	}
	return archive.readBlock(entry.BlockIndex, entry.Name)
}

func (archive *MPQArchive) ReadEntry(entry MPQEntry) ([]byte, error) {
	if archive == nil {
		return nil, fmt.Errorf("MPQ archive is unavailable")
	}
	return archive.readBlock(entry.BlockIndex, entry.Name)
}

func (archive *MPQArchive) loadListFile() error {
	blockIndex, found := archive.findBlockIndex("(listfile)")
	if !found && len(archive.blocks) >= 2 {
		blockIndex = uint32(len(archive.blocks) - 2)
		found = true
	}
	if !found {
		return nil
	}
	data, err := archive.readBlock(blockIndex, "(listfile)")
	if err != nil {
		return fmt.Errorf("read MPQ listfile: %w", err)
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for _, line := range lines {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		blockIndex, found := archive.findBlockIndex(name)
		if !found || blockIndex >= uint32(len(archive.blocks)) {
			continue
		}
		block := archive.blocks[blockIndex]
		entry := MPQEntry{Name: strings.ReplaceAll(name, "\\", "/"), Size: block.uncompressedSize, CompressedSize: block.compressedSize, BlockIndex: blockIndex}
		archive.entries = append(archive.entries, entry)
		archive.byName[mpqNameKey(entry.Name)] = entry
	}
	sort.SliceStable(archive.entries, func(left, right int) bool { return archive.entries[left].Name < archive.entries[right].Name })
	return nil
}

func (archive *MPQArchive) findBlockIndex(name string) (uint32, bool) {
	if archive == nil || len(archive.hashes) == 0 {
		return 0, false
	}
	hashA := mpqHashString(name, mpqHashNameA)
	hashB := mpqHashString(name, mpqHashNameB)
	index := mpqHashString(name, mpqHashTableOffset) & uint32(len(archive.hashes)-1)
	for range archive.hashes {
		hash := archive.hashes[index]
		if hash.blockIndex == mpqHashEntryFree {
			return 0, false
		}
		if hash.locale == 0 && hash.hashA == hashA && hash.hashB == hashB && hash.blockIndex < uint32(len(archive.blocks)) {
			return hash.blockIndex, true
		}
		index = (index + 1) & uint32(len(archive.hashes)-1)
	}
	return 0, false
}

func (archive *MPQArchive) readBlock(blockIndex uint32, name string) ([]byte, error) {
	if blockIndex >= uint32(len(archive.blocks)) {
		return nil, fmt.Errorf("MPQ block index %d is invalid", blockIndex)
	}
	block := archive.blocks[blockIndex]
	if block.flags&mpqFileExists == 0 {
		return nil, fmt.Errorf("MPQ block index %d is not present", blockIndex)
	}
	end := uint64(block.offset) + uint64(block.compressedSize)
	if end > uint64(len(archive.data)) || block.compressedSize == 0 && block.uncompressedSize != 0 {
		return nil, fmt.Errorf("MPQ block index %d is outside the archive", blockIndex)
	}
	fileData := archive.data[block.offset:end]
	key := uint32(0)
	if block.flags&mpqFileEncrypted != 0 {
		key = mpqHashString(name, mpqHashFileKey)
		if block.flags&mpqFileFixKey != 0 {
			key = (key + block.offset) ^ block.uncompressedSize
		}
	}
	if block.flags&mpqFileSingleUnit != 0 {
		if block.flags&mpqFileEncrypted != 0 {
			fileData = mpqDecrypt(fileData, key)
		}
		return mpqDecodeSector(fileData, block.flags, block.uncompressedSize)
	}
	sectorCount := (uint64(block.uncompressedSize) + uint64(archive.sectorSize) - 1) / uint64(archive.sectorSize)
	if sectorCount == 0 {
		return []byte{}, nil
	}
	tableSize := (sectorCount + 1) * 4
	if tableSize > uint64(len(fileData)) {
		return nil, fmt.Errorf("MPQ block index %d has an invalid sector table", blockIndex)
	}
	offsetTable := append([]byte(nil), fileData[:tableSize]...)
	if block.flags&mpqFileEncrypted != 0 {
		offsetTable = mpqDecrypt(offsetTable, key-1)
	}
	offsets := make([]uint32, sectorCount+1)
	for index := range offsets {
		offsets[index] = binary.LittleEndian.Uint32(offsetTable[index*4:])
		if offsets[index] > block.compressedSize || index > 0 && offsets[index] < offsets[index-1] {
			return nil, fmt.Errorf("MPQ block index %d has invalid sector offsets", blockIndex)
		}
	}
	output := make([]byte, 0, block.uncompressedSize)
	for index := uint32(0); index < uint32(sectorCount); index++ {
		start, finish := offsets[index], offsets[index+1]
		if uint64(finish) > uint64(len(fileData)) || finish < start {
			return nil, fmt.Errorf("MPQ block index %d has an invalid sector", blockIndex)
		}
		sector := append([]byte(nil), fileData[start:finish]...)
		if block.flags&mpqFileEncrypted != 0 {
			sector = mpqDecrypt(sector, key+index)
		}
		remaining := int(block.uncompressedSize) - len(output)
		expected := int(archive.sectorSize)
		if remaining < expected {
			expected = remaining
		}
		decoded, err := mpqDecodeSector(sector, block.flags, uint32(expected))
		if err != nil {
			return nil, fmt.Errorf("decode MPQ block %d sector %d: %w", blockIndex, index, err)
		}
		output = append(output, decoded...)
	}
	if len(output) != int(block.uncompressedSize) {
		return nil, fmt.Errorf("MPQ block index %d size mismatch: got %d, want %d", blockIndex, len(output), block.uncompressedSize)
	}
	return output, nil
}

func mpqDecodeSector(data []byte, flags uint32, expected uint32) ([]byte, error) {
	if flags&mpqFileCompress == 0 || uint32(len(data)) == expected {
		return data, nil
	}
	if len(data) >= 2 && data[0] == 0x78 && data[1] == 0x9c {
		return mpqZlib(data)
	}
	if len(data) < 2 || data[0]&mpqCompressionZlib == 0 {
		return data, nil
	}
	return mpqZlib(data[1:])
}

func mpqZlib(data []byte) ([]byte, error) {
	reader, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	decoded, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	return decoded, closeErr
}

func mpqNameKey(name string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
}

func mpqHashString(name string, hashType uint32) uint32 {
	name = strings.ToUpper(strings.ReplaceAll(name, "/", "\\"))
	seed1, seed2 := uint32(0x7fed7fed), uint32(0xeeeeeeee)
	for index := 0; index < len(name); index++ {
		character := name[index]
		seed1 = mpqCryptTable[(hashType<<8)|uint32(character)] ^ (seed1 + seed2)
		seed2 = uint32(character) + seed1 + seed2 + (seed2 << 5) + 3
	}
	return seed1
}

func mpqInitializeCryptTable() {
	seed := uint32(0x00100001)
	for index := uint32(0); index < 0x100; index++ {
		seed = (seed*125 + 3) % 0x2aaaab
		temp1 := (seed & 0xffff) << 16
		seed = (seed*125 + 3) % 0x2aaaab
		temp2 := seed & 0xffff
		mpqCryptTable[index] = temp1 | temp2
		for offset := index + 0x100; offset < 0x500; offset += 0x100 {
			seed = (seed*125 + 3) % 0x2aaaab
			temp1 = (seed & 0xffff) << 16
			seed = (seed*125 + 3) % 0x2aaaab
			temp2 = seed & 0xffff
			mpqCryptTable[offset] = temp1 | temp2
		}
	}
}

func mpqDecrypt(data []byte, key uint32) []byte {
	decoded := append([]byte(nil), data...)
	seed1, seed2 := key, uint32(0xeeeeeeee)
	for offset := 0; offset+4 <= len(decoded); offset += 4 {
		seed2 += mpqCryptTable[0x400+(seed1&0xff)]
		value := binary.LittleEndian.Uint32(decoded[offset:]) ^ (seed1 + seed2)
		binary.LittleEndian.PutUint32(decoded[offset:], value)
		seed1 = ((^seed1 << 21) + 0x11111111) | (seed1 >> 11)
		seed2 = value + seed2 + (seed2 << 5) + 3
	}
	return decoded
}
