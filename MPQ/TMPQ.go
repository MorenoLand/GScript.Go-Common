package mpq

import (
	"bytes"
	"compress/bzip2"
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
	HashTableOffset             = 0
	HashNameA                   = 1
	HashNameB                   = 2
	HashFileKey                 = 3
	FileFlagImplode      uint32 = 0x00000100
	FileFlagCompress     uint32 = 0x00000200
	FileFlagEncrypted    uint32 = 0x00010000
	FileFlagFixKey       uint32 = 0x00020000
	FileFlagPatch        uint32 = 0x00100000
	FileFlagSingleUnit   uint32 = 0x01000000
	FileFlagDeleteMarker uint32 = 0x02000000
	FileFlagSectorCRC    uint32 = 0x04000000
	FileFlagExists       uint32 = 0x80000000
	LocaleNeutral        uint16 = 0
	LocaleAny            uint16 = 0xffff
	maxTableEntries             = 1 << 24
	maxFileSize                 = 1 << 30
)

type MPQEntry struct {
	Name                             string
	Size, CompressedSize, BlockIndex uint32
	Locale, Platform                 uint16
	Flags                            uint32
}
type mpqHashKey struct{ hashA, hashB uint32 }
type mpqHashEntry struct {
	hashA, hashB     uint32
	locale, platform uint16
	block            uint32
}
type mpqBlockEntry struct {
	position                        uint64
	compressedSize, fileSize, flags uint32
}
type MPQArchive struct {
	name               string
	file               *os.File
	reader             io.ReaderAt
	size, headerOffset int64
	sectorSize         uint32
	index              map[mpqHashKey][]mpqHashEntry
	blocks             []mpqBlockEntry
	entries            []MPQEntry
	entriesOnce        sync.Once
}

var mpqCryptTable [0x500]uint32
var mpqCryptTableOnce sync.Once

func OpenMPQ(name string) (*MPQArchive, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	archive, err := newMPQArchive(name, file, info.Size())
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	archive.file = file
	return archive, nil
}
func NewMPQArchive(name string, data []byte) (*MPQArchive, error) {
	copyData := append([]byte(nil), data...)
	return newMPQArchive(name, bytes.NewReader(copyData), int64(len(copyData)))
}

func newMPQArchive(name string, reader io.ReaderAt, size int64) (*MPQArchive, error) {
	headerOffset, err := findMPQHeader(reader, size)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", name, err)
	}
	header, err := readAt(reader, size, headerOffset, 32)
	if err != nil {
		return nil, fmt.Errorf("%q: read MPQ header: %w", name, err)
	}
	headerSize := binary.LittleEndian.Uint32(header[4:8])
	archiveSize := binary.LittleEndian.Uint32(header[8:12])
	formatVersion := binary.LittleEndian.Uint16(header[12:14])
	sectorShift := binary.LittleEndian.Uint16(header[14:16])
	hashPosition := binary.LittleEndian.Uint32(header[16:20])
	blockPosition := binary.LittleEndian.Uint32(header[20:24])
	hashEntries := binary.LittleEndian.Uint32(header[24:28])
	blockEntries := binary.LittleEndian.Uint32(header[28:32])
	if headerSize < 32 || formatVersion > 1 || sectorShift > 20 || hashEntries == 0 || blockEntries == 0 || hashEntries > maxTableEntries || blockEntries > maxTableEntries {
		return nil, fmt.Errorf("%q: unsupported MPQ header version=%d size=%d hash_entries=%d block_entries=%d", name, formatVersion, headerSize, hashEntries, blockEntries)
	}
	if archiveSize < headerSize || uint64(headerOffset)+uint64(archiveSize) > uint64(size) || uint64(headerOffset)+uint64(headerSize) > uint64(size) {
		return nil, fmt.Errorf("%q: archive header exceeds source", name)
	}
	if headerSize > 32 {
		if _, err := readAt(reader, size, headerOffset, int(headerSize)); err != nil {
			return nil, fmt.Errorf("%q: extended MPQ header: %w", name, err)
		}
	}
	archive := &MPQArchive{name: name, reader: reader, size: size, headerOffset: headerOffset, sectorSize: 512 << sectorShift, index: make(map[mpqHashKey][]mpqHashEntry, hashEntries/4), blocks: make([]mpqBlockEntry, blockEntries)}
	mpqCryptTableOnce.Do(mpqInitializeCryptTable)
	hashBytes, err := archive.readRelative(uint64(hashPosition), int(uint64(hashEntries)*16))
	if err != nil {
		return nil, fmt.Errorf("%q: read hash table: %w", name, err)
	}
	decryptMPQ(hashBytes, mpqHashString("(hash table)", HashFileKey))
	for index := uint32(0); index < hashEntries; index++ {
		at := index * 16
		entry := mpqHashEntry{hashA: binary.LittleEndian.Uint32(hashBytes[at:]), hashB: binary.LittleEndian.Uint32(hashBytes[at+4:]), locale: binary.LittleEndian.Uint16(hashBytes[at+8:]), platform: binary.LittleEndian.Uint16(hashBytes[at+10:]), block: binary.LittleEndian.Uint32(hashBytes[at+12:])}
		if entry.block == 0xffffffff || entry.block == 0xfffffffe {
			continue
		}
		key := mpqHashKey{hashA: entry.hashA, hashB: entry.hashB}
		archive.index[key] = append(archive.index[key], entry)
	}
	blockBytes, err := archive.readRelative(uint64(blockPosition), int(uint64(blockEntries)*16))
	if err != nil {
		return nil, fmt.Errorf("%q: read block table: %w", name, err)
	}
	decryptMPQ(blockBytes, mpqHashString("(block table)", HashFileKey))
	var highBlocks []uint16
	if formatVersion >= 1 && headerSize >= 40 {
		headerExt, extErr := archive.readAt(headerOffset+32, int(headerSize)-32)
		if extErr != nil {
			return nil, fmt.Errorf("%q: read MPQ v1 header: %w", name, extErr)
		}
		hiPosition := binary.LittleEndian.Uint64(headerExt[:8])
		if hiPosition != 0 {
			hiData, hiErr := archive.readRelative(hiPosition, int(uint64(blockEntries)*2))
			if hiErr != nil {
				return nil, fmt.Errorf("%q: read high block table: %w", name, hiErr)
			}
			highBlocks = make([]uint16, blockEntries)
			for index := range highBlocks {
				highBlocks[index] = binary.LittleEndian.Uint16(hiData[index*2:])
			}
		}
	}
	for index := uint32(0); index < blockEntries; index++ {
		at := index * 16
		position := uint64(binary.LittleEndian.Uint32(blockBytes[at:]))
		if int(index) < len(highBlocks) {
			position |= uint64(highBlocks[index]) << 32
		}
		archive.blocks[index] = mpqBlockEntry{position: position, compressedSize: binary.LittleEndian.Uint32(blockBytes[at+4:]), fileSize: binary.LittleEndian.Uint32(blockBytes[at+8:]), flags: binary.LittleEndian.Uint32(blockBytes[at+12:])}
	}
	return archive, nil
}

func (archive *MPQArchive) Name() string {
	if archive == nil {
		return ""
	}
	return archive.name
}
func (archive *MPQArchive) Close() error {
	if archive == nil || archive.file == nil {
		return nil
	}
	err := archive.file.Close()
	archive.file = nil
	return err
}
func (archive *MPQArchive) Entries() []MPQEntry {
	if archive == nil {
		return nil
	}
	archive.entriesOnce.Do(func() { archive.loadListFile() })
	return append([]MPQEntry(nil), archive.entries...)
}

func (archive *MPQArchive) Find(name string, locale uint16) (MPQEntry, bool) {
	if archive == nil || len(archive.index) == 0 {
		return MPQEntry{}, false
	}
	name = NormalizePath(name)
	key := mpqHashKey{hashA: mpqHashString(name, HashNameA), hashB: mpqHashString(name, HashNameB)}
	entries := archive.index[key]
	if len(entries) == 0 {
		return MPQEntry{}, false
	}
	for _, wantedLocale := range []uint16{locale, LocaleNeutral, LocaleAny} {
		for _, entry := range entries {
			if (wantedLocale != LocaleAny && entry.locale != wantedLocale) || entry.block >= uint32(len(archive.blocks)) {
				continue
			}
			block := archive.blocks[entry.block]
			return MPQEntry{Name: name, Size: block.fileSize, CompressedSize: block.compressedSize, BlockIndex: entry.block, Locale: entry.locale, Platform: entry.platform, Flags: block.flags}, true
		}
	}
	return MPQEntry{}, false
}

func (archive *MPQArchive) ReadFile(name string) ([]byte, error) {
	return archive.ReadFileLocale(name, LocaleNeutral)
}
func (archive *MPQArchive) ReadFileLocale(name string, locale uint16) ([]byte, error) {
	entry, ok := archive.Find(name, locale)
	if !ok {
		return nil, fmt.Errorf("MPQ entry %q was not found", name)
	}
	return archive.ReadEntry(entry)
}

func (archive *MPQArchive) ReadEntry(entry MPQEntry) ([]byte, error) {
	if archive == nil {
		return nil, fmt.Errorf("MPQ archive is unavailable")
	}
	if entry.BlockIndex >= uint32(len(archive.blocks)) {
		return nil, fmt.Errorf("MPQ block index %d is invalid", entry.BlockIndex)
	}
	block := archive.blocks[entry.BlockIndex]
	if block.flags&FileFlagDeleteMarker != 0 || block.flags&FileFlagPatch != 0 {
		return nil, os.ErrNotExist
	}
	if block.fileSize > maxFileSize || block.compressedSize > maxFileSize {
		return nil, fmt.Errorf("MPQ file %s is too large", entry.Name)
	}
	if block.position+uint64(block.compressedSize) > uint64(archive.size) || block.compressedSize == 0 && block.fileSize != 0 {
		return nil, fmt.Errorf("MPQ block index %d is outside the archive", entry.BlockIndex)
	}
	keyName := NormalizePath(entry.Name)
	if separator := strings.LastIndex(keyName, "\\"); separator >= 0 {
		keyName = keyName[separator+1:]
	}
	key := mpqHashString(keyName, HashFileKey)
	if block.flags&FileFlagFixKey != 0 {
		key = (key + uint32(block.position)) ^ block.fileSize
	}
	if block.flags&FileFlagSingleUnit != 0 {
		data, err := archive.readAt(archive.headerOffset+int64(block.position), int(block.compressedSize))
		if err != nil {
			return nil, err
		}
		if block.flags&FileFlagEncrypted != 0 {
			decryptMPQ(data, key)
		}
		return decodeMPQSector(data, int(block.fileSize), block.flags&(FileFlagCompress|FileFlagImplode) != 0)
	}
	sectorSize := uint64(archive.sectorSize)
	sectorCount := (uint64(block.fileSize) + sectorSize - 1) / sectorSize
	if sectorCount == 0 {
		return []byte{}, nil
	}
	tableSize := (sectorCount + 1) * 4
	if block.flags&FileFlagSectorCRC != 0 {
		tableSize += sectorCount * 4
	}
	if tableSize > uint64(block.compressedSize) {
		return nil, fmt.Errorf("MPQ block index %d has an invalid sector table", entry.BlockIndex)
	}
	offsetData, err := archive.readAt(archive.headerOffset+int64(block.position), int(tableSize))
	if err != nil {
		return nil, err
	}
	if block.flags&FileFlagEncrypted != 0 {
		decryptMPQ(offsetData, key-1)
	}
	offsets := make([]uint32, sectorCount+1)
	for index := range offsets {
		offsets[index] = binary.LittleEndian.Uint32(offsetData[index*4:])
		if offsets[index] > block.compressedSize || index > 0 && offsets[index] < offsets[index-1] {
			return nil, fmt.Errorf("MPQ block index %d has invalid sector offsets", entry.BlockIndex)
		}
	}
	output := make([]byte, 0, block.fileSize)
	for index := uint64(0); index < sectorCount; index++ {
		start, finish := offsets[index], offsets[index+1]
		if finish < start || uint64(finish) > uint64(block.compressedSize) {
			return nil, fmt.Errorf("MPQ block index %d has an invalid sector", entry.BlockIndex)
		}
		sector, readErr := archive.readAt(archive.headerOffset+int64(block.position)+int64(start), int(finish-start))
		if readErr != nil {
			return nil, readErr
		}
		if block.flags&FileFlagEncrypted != 0 {
			decryptMPQ(sector, key+uint32(index))
		}
		remaining := int(block.fileSize) - len(output)
		expected := int(sectorSize)
		if remaining < expected {
			expected = remaining
		}
		decoded, decodeErr := decodeMPQSector(sector, expected, block.flags&(FileFlagCompress|FileFlagImplode) != 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("decode MPQ block %d sector %d: %w", entry.BlockIndex, index, decodeErr)
		}
		output = append(output, decoded...)
	}
	if len(output) != int(block.fileSize) {
		return nil, fmt.Errorf("MPQ block index %d size mismatch: got %d, want %d", entry.BlockIndex, len(output), block.fileSize)
	}
	return output, nil
}

func (archive *MPQArchive) loadListFile() {
	entry, ok := archive.Find("(listfile)", LocaleNeutral)
	if !ok {
		return
	}
	data, err := archive.ReadEntry(entry)
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		if listed, found := archive.Find(name, LocaleNeutral); found {
			archive.entries = append(archive.entries, listed)
		}
	}
	sort.SliceStable(archive.entries, func(left, right int) bool { return archive.entries[left].Name < archive.entries[right].Name })
}
func (archive *MPQArchive) readRelative(offset uint64, size int) ([]byte, error) {
	return archive.readAt(archive.headerOffset+int64(offset), size)
}
func (archive *MPQArchive) readAt(offset int64, size int) ([]byte, error) {
	return readAt(archive.reader, archive.size, offset, size)
}
func readAt(reader io.ReaderAt, sourceSize, offset int64, size int) ([]byte, error) {
	if offset < 0 || size < 0 || offset > sourceSize || int64(size) > sourceSize-offset {
		return nil, io.ErrUnexpectedEOF
	}
	data := make([]byte, size)
	read, err := reader.ReadAt(data, offset)
	if err != nil {
		return nil, err
	}
	if read != size {
		return nil, io.ErrUnexpectedEOF
	}
	return data, nil
}
func findMPQHeader(reader io.ReaderAt, sourceSize int64) (int64, error) {
	limit := sourceSize
	if limit > 1<<20 {
		limit = 1 << 20
	}
	for offset := int64(0); offset+4 <= limit; offset += 0x200 {
		magic, err := readAt(reader, sourceSize, offset, 4)
		if err != nil {
			return 0, err
		}
		if string(magic) == "MPQ\x1a" {
			return offset, nil
		}
	}
	return 0, fmt.Errorf("MPQ header not found")
}

func decodeMPQSector(data []byte, expected int, compressed bool) ([]byte, error) {
	if !compressed {
		if len(data) < expected {
			return nil, io.ErrUnexpectedEOF
		}
		return append([]byte(nil), data[:expected]...), nil
	}
	if len(data) == expected {
		return append([]byte(nil), data...), nil
	}
	if len(data) < 1 {
		return nil, io.ErrUnexpectedEOF
	}
	mask, payload := data[0], data[1:]
	var reader io.Reader
	switch {
	case mask&0x02 != 0:
		zreader, err := zlib.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		defer zreader.Close()
		reader = zreader
	case mask&0x10 != 0:
		reader = bzip2.NewReader(bytes.NewReader(payload))
	case mask&0x08 != 0:
		return nil, fmt.Errorf("PKWARE implode compression is not supported")
	default:
		zreader, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer zreader.Close()
		reader = zreader
	}
	decoded, err := io.ReadAll(io.LimitReader(reader, int64(expected)+1))
	if err != nil {
		return nil, err
	}
	if len(decoded) < expected {
		return nil, io.ErrUnexpectedEOF
	}
	if len(decoded) > expected {
		decoded = decoded[:expected]
	}
	return decoded, nil
}

func NormalizePath(name string) string {
	return strings.TrimPrefix(strings.ReplaceAll(name, "/", "\\"), "\\")
}
func HashString(name string, hashType int) uint32 { return mpqHashString(name, hashType) }
func mpqHashString(name string, hashType int) uint32 {
	name = strings.ToUpper(NormalizePath(name))
	seed1, seed2 := uint32(0x7fed7fed), uint32(0xeeeeeeee)
	for index := 0; index < len(name); index++ {
		character := name[index]
		seed1 = mpqCryptTable[(hashType<<8)|int(character)] ^ (seed1 + seed2)
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
func decryptMPQ(data []byte, key uint32) {
	seed1, seed2 := key, uint32(0xeeeeeeee)
	for offset := 0; offset+4 <= len(data); offset += 4 {
		seed2 += mpqCryptTable[0x400+(seed1&0xff)]
		value := binary.LittleEndian.Uint32(data[offset:]) ^ (seed1 + seed2)
		binary.LittleEndian.PutUint32(data[offset:], value)
		seed1 = ((^seed1 << 21) + 0x11111111) | (seed1 >> 11)
		seed2 = value + seed2 + (seed2 << 5) + 3
	}
}
