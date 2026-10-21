package runtime

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"testing"

	"github.com/vortelio/vortelio/internal/hub"
)

func TestResolveVideoRequest(t *testing.T) {
	m := &hub.Model{Type: "video", LocalPath: "Wan2.1-T2V-1.3B-Q8_0.gguf"}
	r := ResolveVideoRequest(m, VideoRequest{Prompt: "x", Frames: 30, Width: 500, InitImage: []byte{1}})
	if r.Frames != 29 || r.Width != 512 || r.Height != 288 || r.FPS != 16 || r.Steps != 20 || r.CFG != 6 {
		t.Errorf("wan2.1: %+v", r)
	}
	if r.Negative == "" || r.InitImage != nil {
		t.Error("default negative missing or start image kept for a text-only model")
	}
	m22 := &hub.Model{Type: "video", LocalPath: "Wan2.2-TI2V-5B-Q4_K_S.gguf"}
	r = ResolveVideoRequest(m22, VideoRequest{Prompt: "x", Frames: 500, InitImage: []byte{1}})
	if r.Frames != 121 || r.FPS != 24 || r.InitImage == nil {
		t.Errorf("wan2.2: %+v", r)
	}
}

// buildAVI makes a minimal MJPEG AVI the way the engine writes one.
func buildAVI(frames [][]byte) []byte {
	var movi bytes.Buffer
	movi.WriteString("movi")
	for _, f := range frames {
		movi.WriteString("00dc")
		binary.Write(&movi, binary.LittleEndian, uint32(len(f)))
		movi.Write(f)
		if len(f)%2 == 1 {
			movi.WriteByte(0)
		}
	}
	var avi bytes.Buffer
	avi.WriteString("RIFF")
	binary.Write(&avi, binary.LittleEndian, uint32(0))
	avi.WriteString("AVI LIST")
	binary.Write(&avi, binary.LittleEndian, uint32(movi.Len()))
	avi.Write(movi.Bytes())
	avi.WriteString("idx1")
	binary.Write(&avi, binary.LittleEndian, uint32(0))
	return avi.Bytes()
}

func TestAVIToGIF(t *testing.T) {
	var frames [][]byte
	for i := 0; i < 3; i++ {
		img := image.NewRGBA(image.Rect(0, 0, 32, 16))
		for x := 0; x < 32; x++ {
			img.Set(x, 0, color.RGBA{uint8(80 * i), 0, 0, 255})
		}
		var b bytes.Buffer
		jpeg.Encode(&b, img, nil)
		frames = append(frames, b.Bytes())
	}
	avi := buildAVI(frames)
	got, err := aviFrames(avi)
	if err != nil || len(got) != 3 {
		t.Fatalf("frames = %d, %v", len(got), err)
	}
	data, err := aviToGIF(avi, 16)
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil || len(g.Image) != 3 || g.Delay[0] != 6 {
		t.Fatalf("gif: %v frames=%d", err, len(g.Image))
	}
}
