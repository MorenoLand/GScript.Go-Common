package mpq

import (
	"bytes"
	"compress/zlib"
	"io"
	"testing"
)

func TestNormalizePath(t *testing.T) {
	if got := NormalizePath(`\Interface/GlueXML/AccountLogin.xml`); got != `Interface\GlueXML\AccountLogin.xml` {
		t.Fatalf("path=%q", got)
	}
}

func TestFileKeyUsesBasename(t *testing.T) {
	if HashString("AccountLogin.xml", HashFileKey) == HashString(`Interface\GlueXML\AccountLogin.xml`, HashFileKey) {
		t.Fatal("basename and full-path keys must differ")
	}
}

func TestDecodeMPQSectorZlib(t *testing.T) {
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	if _, err := writer.Write([]byte("shared MPQ data")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data := append([]byte{0x02}, compressed.Bytes()...)
	decoded, err := decodeMPQSector(data, len("shared MPQ data"), true)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != "shared MPQ data" {
		t.Fatalf("decoded=%q", decoded)
	}
	if _, err := io.ReadAll(bytes.NewReader(decoded)); err != nil {
		t.Fatal(err)
	}
}
