// Package gguf reads the header of a GGUF model file: metadata key/values and
// tensor descriptions, without touching the weights.
//
// Format: https://github.com/ggml-org/ggml/blob/master/docs/gguf.md
//
//	magic "GGUF" | version u32 | tensor_count u64 | kv_count u64
//	kv[]     key:string, type:u32, value
//	tensor[] name:string, n_dims:u32, dims[n_dims]:u64, type:u32, offset:u64
package gguf

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

const (
	typeUInt8   = 0
	typeInt8    = 1
	typeUInt16  = 2
	typeInt16   = 3
	typeUInt32  = 4
	typeInt32   = 5
	typeFloat32 = 6
	typeBool    = 7
	typeString  = 8
	typeArray   = 9
	typeUInt64  = 10
	typeInt64   = 11
	typeFloat64 = 12
)

// MaxArrayKept is the longest array value kept in Metadata; longer arrays
// (tokenizer vocabularies…) are read past and recorded as ArrayInfo.
const MaxArrayKept = 1024

// ArrayInfo stands in for an array too long to keep.
type ArrayInfo struct {
	Type   uint32 `json:"type"`
	Length uint64 `json:"length"`
}

// Tensor describes one tensor (no data).
type Tensor struct {
	Name string   `json:"name"`
	Dims []uint64 `json:"dims"`
	Type uint32   `json:"type"`
}

// File is a parsed GGUF header.
type File struct {
	Version     uint32
	TensorCount uint64
	Metadata    map[string]any
	Tensors     []Tensor // up to the limit passed to Read
}

// Read parses the header of the GGUF at path, keeping at most maxTensors
// tensor descriptions (0 = none).
func Read(path string, maxTensors int) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f, maxTensors)
}

// Parse reads a GGUF header from r.
func Parse(r io.Reader, maxTensors int) (*File, error) {
	g := &reader{r: bufio.NewReaderSize(r, 1<<20)}
	var magic [4]byte
	if _, err := io.ReadFull(g.r, magic[:]); err != nil {
		return nil, err
	}
	if string(magic[:]) != "GGUF" {
		return nil, fmt.Errorf("not a GGUF file (magic=%q)", magic[:])
	}
	out := &File{Metadata: map[string]any{}}
	out.Version = g.u32()
	out.TensorCount = g.u64()
	kvCount := g.u64()
	if g.err != nil {
		return nil, g.err
	}
	if kvCount > 1<<20 {
		return nil, errors.New("corrupt GGUF: too many metadata keys")
	}
	for i := uint64(0); i < kvCount; i++ {
		key := g.str()
		val := g.valueOfType(g.u32())
		if g.err != nil {
			return out, fmt.Errorf("metadata %q: %w", key, g.err)
		}
		out.Metadata[key] = val
	}
	for i := uint64(0); i < out.TensorCount && i < uint64(maxTensors) && g.err == nil; i++ {
		t := Tensor{Name: g.str()}
		n := g.u32()
		if n > 8 {
			return out, errors.New("corrupt GGUF: tensor with too many dimensions")
		}
		for j := uint32(0); j < n; j++ {
			t.Dims = append(t.Dims, g.u64())
		}
		t.Type = g.u32()
		_ = g.u64() // data offset
		out.Tensors = append(out.Tensors, t)
	}
	return out, g.err
}

// Arch is general.architecture ("llama", "qwen35", "gemma4"…).
func (f *File) Arch() string { return f.String("general.architecture") }

// String returns a string value ("" when missing).
func (f *File) String(key string) string {
	s, _ := f.Metadata[key].(string)
	return s
}

// Int returns an integer value of any width.
func (f *File) Int(key string) (int64, bool) {
	switch v := f.Metadata[key].(type) {
	case uint8:
		return int64(v), true
	case int8:
		return int64(v), true
	case uint16:
		return int64(v), true
	case int16:
		return int64(v), true
	case uint32:
		return int64(v), true
	case int32:
		return int64(v), true
	case uint64:
		return int64(v), true
	case int64:
		return v, true
	}
	return 0, false
}

// ArchInt returns "<arch>.<suffix>" as an integer (e.g. "block_count").
func (f *File) ArchInt(suffix string) (int64, bool) { return f.Int(f.Arch() + "." + suffix) }

type reader struct {
	r   *bufio.Reader
	err error
	buf [8]byte
}

func (g *reader) read(n int) []byte {
	if g.err != nil {
		return g.buf[:n]
	}
	if _, err := io.ReadFull(g.r, g.buf[:n]); err != nil {
		g.err = err
	}
	return g.buf[:n]
}

func (g *reader) u8() uint8   { return g.read(1)[0] }
func (g *reader) u16() uint16 { return binary.LittleEndian.Uint16(g.read(2)) }
func (g *reader) u32() uint32 { return binary.LittleEndian.Uint32(g.read(4)) }
func (g *reader) u64() uint64 { return binary.LittleEndian.Uint64(g.read(8)) }

func (g *reader) str() string {
	n := g.u64()
	if g.err != nil {
		return ""
	}
	if n > 64<<20 {
		g.err = errors.New("corrupt GGUF: string too long")
		return ""
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(g.r, b); err != nil {
		g.err = err
		return ""
	}
	return string(b)
}

func (g *reader) skipStr() {
	n := g.u64()
	if g.err != nil {
		return
	}
	if _, err := g.r.Discard(int(n)); err != nil {
		g.err = err
	}
}

var fixedSize = map[uint32]int{
	typeUInt8: 1, typeInt8: 1, typeBool: 1,
	typeUInt16: 2, typeInt16: 2,
	typeUInt32: 4, typeInt32: 4, typeFloat32: 4,
	typeUInt64: 8, typeInt64: 8, typeFloat64: 8,
}

func (g *reader) valueOfType(t uint32) any {
	switch t {
	case typeUInt8:
		return g.u8()
	case typeInt8:
		return int8(g.u8())
	case typeUInt16:
		return g.u16()
	case typeInt16:
		return int16(g.u16())
	case typeUInt32:
		return g.u32()
	case typeInt32:
		return int32(g.u32())
	case typeFloat32:
		return math.Float32frombits(g.u32())
	case typeBool:
		return g.u8() != 0
	case typeString:
		return g.str()
	case typeUInt64:
		return g.u64()
	case typeInt64:
		return int64(g.u64())
	case typeFloat64:
		return math.Float64frombits(g.u64())
	case typeArray:
		sub := g.u32()
		n := g.u64()
		if g.err != nil {
			return nil
		}
		if n <= MaxArrayKept {
			out := make([]any, 0, n)
			for i := uint64(0); i < n && g.err == nil; i++ {
				out = append(out, g.valueOfType(sub))
			}
			return out
		}
		// Too long to keep: skip it correctly so later keys stay aligned.
		if sz, ok := fixedSize[sub]; ok {
			if _, err := g.r.Discard(int(n) * sz); err != nil {
				g.err = err
			}
		} else {
			for i := uint64(0); i < n && g.err == nil; i++ {
				if sub == typeString {
					g.skipStr()
				} else {
					g.valueOfType(sub)
				}
			}
		}
		return ArrayInfo{Type: sub, Length: n}
	}
	g.err = fmt.Errorf("unknown GGUF value type %d", t)
	return nil
}
