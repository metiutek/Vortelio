package gguf

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func w(b *bytes.Buffer, v any) { binary.Write(b, binary.LittleEndian, v) }
func ws(b *bytes.Buffer, s string) {
	w(b, uint64(len(s)))
	b.WriteString(s)
}

// A vocabulary longer than MaxArrayKept must be skipped exactly, so the keys
// after it are still read correctly.
func TestLongArraySkippedAligned(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("GGUF")
	w(&b, uint32(3))
	w(&b, uint64(0))
	w(&b, uint64(3))
	ws(&b, "general.architecture")
	w(&b, uint32(typeString))
	ws(&b, "qwen35")
	ws(&b, "tokenizer.ggml.tokens")
	w(&b, uint32(typeArray))
	w(&b, uint32(typeString))
	w(&b, uint64(5000))
	for i := 0; i < 5000; i++ {
		ws(&b, "tok")
	}
	ws(&b, "qwen35.pooling_type")
	w(&b, uint32(typeUInt32))
	w(&b, uint32(1))
	f, err := Parse(&b, 0)
	if err != nil {
		t.Fatal(err)
	}
	if f.Arch() != "qwen35" {
		t.Fatal(f.Arch())
	}
	if v, ok := f.ArchInt("pooling_type"); !ok || v != 1 {
		t.Fatalf("pooling_type after long array: %v %v", v, ok)
	}
	if ai, ok := f.Metadata["tokenizer.ggml.tokens"].(ArrayInfo); !ok || ai.Length != 5000 {
		t.Fatalf("%#v", f.Metadata["tokenizer.ggml.tokens"])
	}
}
