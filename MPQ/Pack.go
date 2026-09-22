package mpq

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"strings"
)

const mpqPackHashEntryFree uint32 = 0xffffffff

type MPQPackFile struct {
	Name     string
	Data     []byte
	Locale   uint16
	Platform uint16
}

type MPQPackOptions struct {
	Compress        bool
	IncludeListFile bool
}

type mpqPackEntry struct {
	name     string
	data     []byte
	stored   []byte
	locale   uint16
	platform uint16
	flags    uint32
	offset   uint32
}

func PackMPQ(files []MPQPackFile) ([]byte, error) {
	return PackMPQWithOptions(files, MPQPackOptions{Compress: true, IncludeListFile: true})
}

func PackMPQWithOptions(files []MPQPackFile, options MPQPackOptions) ([]byte, error) {
	entries := make([]mpqPackEntry, 0, len(files)+1)
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		name, err := normalizeMPQPackName(file.Name)
		if err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%s\x00%d\x00%d", strings.ToLower(name), file.Locale, file.Platform)
		if seen[key] {
			return nil, fmt.Errorf("duplicate MPQ entry %q", name)
		}
		seen[key] = true
		stored, compressed := packMPQData(file.Data, options.Compress)
		flags := uint32(FileFlagSingleUnit | FileFlagExists)
		if compressed {
			flags |= FileFlagCompress
		}
		entries = append(entries, mpqPackEntry{name: name, data: append([]byte(nil), file.Data...), stored: stored, locale: file.Locale, platform: file.Platform, flags: flags})
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("MPQ contains no files")
	}
	sort.SliceStable(entries, func(left, right int) bool { return entries[left].name < entries[right].name })
	if options.IncludeListFile {
		list := make([]string, len(entries))
		for index, entry := range entries {
			list[index] = entry.name
		}
		data := []byte(strings.Join(list, "\n") + "\n")
		entries = append([]mpqPackEntry{{name: "(listfile)", data: data, stored: data, flags: FileFlagSingleUnit | FileFlagExists}}, entries...)
	}
	hashCount := 4
	for hashCount < len(entries)*2 {
		hashCount <<= 1
	}
	const headerSize = uint32(32)
	hashPosition := headerSize
	blockPosition := hashPosition + uint32(hashCount*16)
	dataPosition := blockPosition + uint32(len(entries)*16)
	dataEnd := dataPosition
	for index := range entries {
		entries[index].offset = dataEnd
		dataEnd += uint32(len(entries[index].stored))
	}
	if uint64(dataEnd) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("MPQ is larger than the v0 archive limit")
	}
	cryptInitializeForPack()
	hashTable := make([]byte, hashCount*16)
	for index := 0; index < hashCount; index++ {
		binary.LittleEndian.PutUint32(hashTable[index*16+12:], mpqPackHashEntryFree)
	}
	for index, entry := range entries {
		if !insertMPQPackHash(hashTable, hashCount, entry.name, entry.locale, entry.platform, uint32(index)) {
			return nil, fmt.Errorf("MPQ hash table is full")
		}
	}
	blockTable := make([]byte, len(entries)*16)
	for index, entry := range entries {
		offset := index * 16
		binary.LittleEndian.PutUint32(blockTable[offset:], entry.offset)
		binary.LittleEndian.PutUint32(blockTable[offset+4:], uint32(len(entry.stored)))
		binary.LittleEndian.PutUint32(blockTable[offset+8:], uint32(len(entry.data)))
		binary.LittleEndian.PutUint32(blockTable[offset+12:], entry.flags)
	}
	encryptMPQPack(hashTable, mpqHashString("(hash table)", HashFileKey))
	encryptMPQPack(blockTable, mpqHashString("(block table)", HashFileKey))
	header := make([]byte, headerSize)
	copy(header[:4], []byte{'M', 'P', 'Q', 0x1a})
	binary.LittleEndian.PutUint32(header[4:], headerSize)
	binary.LittleEndian.PutUint32(header[8:], dataEnd)
	binary.LittleEndian.PutUint16(header[12:], 0)
	binary.LittleEndian.PutUint16(header[14:], 3)
	binary.LittleEndian.PutUint32(header[16:], hashPosition)
	binary.LittleEndian.PutUint32(header[20:], blockPosition)
	binary.LittleEndian.PutUint32(header[24:], uint32(hashCount))
	binary.LittleEndian.PutUint32(header[28:], uint32(len(entries)))
	archive := make([]byte, 0, dataEnd)
	archive = append(archive, header...)
	archive = append(archive, hashTable...)
	archive = append(archive, blockTable...)
	for _, entry := range entries {
		archive = append(archive, entry.stored...)
	}
	return archive, nil
}

func WriteMPQ(name string, files []MPQPackFile) error {
	data, err := PackMPQ(files)
	if err != nil {
		return err
	}
	return os.WriteFile(name, data, 0644)
}

func normalizeMPQPackName(name string) (string, error) {
	name = NormalizePath(name)
	if name == "" || strings.EqualFold(name, "(listfile)") || strings.Contains(name, ":") {
		return "", fmt.Errorf("invalid MPQ entry name %q", name)
	}
	for _, part := range strings.Split(name, "\\") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("unsafe MPQ entry name %q", name)
		}
	}
	return name, nil
}

func packMPQData(data []byte, compress bool) ([]byte, bool) {
	if !compress || len(data) == 0 {
		return append([]byte(nil), data...), false
	}
	var buffer bytes.Buffer
	writer := zlib.NewWriter(&buffer)
	if _, err := writer.Write(data); err != nil || writer.Close() != nil || buffer.Len()+1 >= len(data) {
		return append([]byte(nil), data...), false
	}
	stored := make([]byte, 1, buffer.Len()+1)
	stored[0] = 0x02
	stored = append(stored, buffer.Bytes()...)
	return stored, true
}

func insertMPQPackHash(table []byte, count int, name string, locale, platform uint16, block uint32) bool {
	hashA := mpqHashString(name, HashNameA)
	hashB := mpqHashString(name, HashNameB)
	index := mpqHashString(name, HashTableOffset) & uint32(count-1)
	for range count {
		offset := int(index) * 16
		if binary.LittleEndian.Uint32(table[offset+12:]) == mpqPackHashEntryFree {
			binary.LittleEndian.PutUint32(table[offset:], hashA)
			binary.LittleEndian.PutUint32(table[offset+4:], hashB)
			binary.LittleEndian.PutUint16(table[offset+8:], locale)
			binary.LittleEndian.PutUint16(table[offset+10:], platform)
			binary.LittleEndian.PutUint32(table[offset+12:], block)
			return true
		}
		index = (index + 1) & uint32(count-1)
	}
	return false
}

func cryptInitializeForPack() {
	mpqCryptTableOnce.Do(mpqInitializeCryptTable)
}

func encryptMPQPack(data []byte, key uint32) {
	seed1, seed2 := key, uint32(0xeeeeeeee)
	for offset := 0; offset+4 <= len(data); offset += 4 {
		seed2 += mpqCryptTable[0x400+(seed1&0xff)]
		plain := binary.LittleEndian.Uint32(data[offset:])
		value := plain ^ (seed1 + seed2)
		binary.LittleEndian.PutUint32(data[offset:], value)
		seed1 = ((^seed1 << 21) + 0x11111111) | (seed1 >> 11)
		seed2 = plain + seed2 + (seed2 << 5) + 3
	}
}
