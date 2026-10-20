package runtime

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"html"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Audio inputs. The engines read 16-bit PCM WAV; stem separation needs
// 44.1 kHz stereo. WAV input is converted in Go; other formats (mp3, m4a,
// flac…) go through ffmpeg when it is installed. The web UI decodes any
// format in the browser and always sends WAV.

// PCM is decoded audio: interleaved float samples in [-1, 1].
type PCM struct {
	Rate     int
	Channels int
	Samples  []float32
}

// Seconds is the duration of the audio.
func (p *PCM) Seconds() float64 {
	if p.Rate == 0 || p.Channels == 0 {
		return 0
	}
	return float64(len(p.Samples)) / float64(p.Channels) / float64(p.Rate)
}

// DecodeWAV reads PCM (8/16/24/32-bit integer or 32/64-bit float) WAV data.
func DecodeWAV(data []byte) (*PCM, error) {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, errors.New("not a WAV file")
	}
	var format, channels, bits uint16
	var rate uint32
	var pcm []byte
	for off := 12; off+8 <= len(data); {
		id := string(data[off : off+4])
		size := int(binary.LittleEndian.Uint32(data[off+4 : off+8]))
		body := data[off+8:]
		if size > len(body) {
			size = len(body) // truncated / streaming header: take what is there
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return nil, errors.New("bad WAV format chunk")
			}
			format = binary.LittleEndian.Uint16(body[0:2])
			channels = binary.LittleEndian.Uint16(body[2:4])
			rate = binary.LittleEndian.Uint32(body[4:8])
			bits = binary.LittleEndian.Uint16(body[14:16])
			if format == 0xFFFE && size >= 26 { // WAVE_FORMAT_EXTENSIBLE
				format = binary.LittleEndian.Uint16(body[24:26])
			}
		case "data":
			pcm = body[:size]
		}
		off += 8 + size + size%2
	}
	if channels == 0 || rate == 0 || pcm == nil {
		return nil, errors.New("WAV without audio data")
	}
	bps := int(bits) / 8
	if bps == 0 {
		return nil, errors.New("bad WAV sample size")
	}
	n := len(pcm) / bps
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		b := pcm[i*bps : i*bps+bps]
		switch {
		case format == 3 && bits == 32:
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b))
		case format == 3 && bits == 64:
			out[i] = float32(math.Float64frombits(binary.LittleEndian.Uint64(b)))
		case bits == 8:
			out[i] = (float32(b[0]) - 128) / 128
		case bits == 16:
			out[i] = float32(int16(binary.LittleEndian.Uint16(b))) / 32768
		case bits == 24:
			v := int32(b[0]) | int32(b[1])<<8 | int32(int8(b[2]))<<16
			out[i] = float32(v) / 8388608
		case bits == 32:
			out[i] = float32(int32(binary.LittleEndian.Uint32(b))) / 2147483648
		default:
			return nil, fmt.Errorf("unsupported WAV encoding (format %d, %d bit)", format, bits)
		}
	}
	return &PCM{Rate: int(rate), Channels: int(channels), Samples: out}, nil
}

// EncodeWAV writes 16-bit PCM WAV.
func EncodeWAV(p *PCM) []byte {
	var b bytes.Buffer
	dataLen := len(p.Samples) * 2
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+dataLen))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint16(p.Channels))
	binary.Write(&b, binary.LittleEndian, uint32(p.Rate))
	binary.Write(&b, binary.LittleEndian, uint32(p.Rate*p.Channels*2))
	binary.Write(&b, binary.LittleEndian, uint16(p.Channels*2))
	binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(dataLen))
	for _, s := range p.Samples {
		if s > 1 {
			s = 1
		} else if s < -1 {
			s = -1
		}
		binary.Write(&b, binary.LittleEndian, int16(math.Round(float64(s)*32767)))
	}
	return b.Bytes()
}

// Convert returns p resampled to rate (0 = keep) with channels (0 = keep).
func (p *PCM) Convert(rate, channels int) *PCM {
	src := p
	if channels > 0 && channels != p.Channels {
		frames := len(p.Samples) / p.Channels
		mixed := make([]float32, frames*channels)
		for f := 0; f < frames; f++ {
			var sum float32
			for c := 0; c < p.Channels; c++ {
				sum += p.Samples[f*p.Channels+c]
			}
			mono := sum / float32(p.Channels)
			for c := 0; c < channels; c++ {
				if p.Channels == channels {
					mixed[f*channels+c] = p.Samples[f*p.Channels+c]
				} else if p.Channels > 1 && channels > 1 && c < p.Channels {
					mixed[f*channels+c] = p.Samples[f*p.Channels+c]
				} else {
					mixed[f*channels+c] = mono
				}
			}
		}
		src = &PCM{Rate: p.Rate, Channels: channels, Samples: mixed}
	}
	if rate <= 0 || rate == src.Rate {
		return src
	}
	// Linear interpolation; adequate for speech and for model inputs, which
	// are resampled again internally.
	ch := src.Channels
	inFrames := len(src.Samples) / ch
	outFrames := int(int64(inFrames) * int64(rate) / int64(src.Rate))
	out := make([]float32, outFrames*ch)
	step := float64(src.Rate) / float64(rate)
	for f := 0; f < outFrames; f++ {
		pos := float64(f) * step
		i := int(pos)
		frac := float32(pos - float64(i))
		for c := 0; c < ch; c++ {
			a := src.Samples[min(i, inFrames-1)*ch+c]
			b := src.Samples[min(i+1, inFrames-1)*ch+c]
			out[f*ch+c] = a + (b-a)*frac
		}
	}
	return &PCM{Rate: rate, Channels: ch, Samples: out}
}

// FindFFmpeg returns an ffmpeg binary or "".
func FindFFmpeg() string {
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	return ""
}

// PrepareAudioInput turns uploaded audio (any format) into a 16-bit WAV at
// rate/channels (0 = keep) inside dir, and returns its path and decoded form.
func PrepareAudioInput(ctx context.Context, data []byte, dir string, rate, channels int) (string, *PCM, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	pcm, err := DecodeWAV(data)
	if err != nil {
		ff := FindFFmpeg()
		if ff == "" {
			return "", nil, errors.New("this audio format needs ffmpeg (or upload a WAV file)")
		}
		in := filepath.Join(dir, "input.bin")
		if err := os.WriteFile(in, data, 0o644); err != nil {
			return "", nil, err
		}
		wav := filepath.Join(dir, "input-ffmpeg.wav")
		cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		args := []string{"-y", "-loglevel", "error", "-i", in, "-vn", "-acodec", "pcm_s16le"}
		if rate > 0 {
			args = append(args, "-ar", fmt.Sprint(rate))
		}
		if channels > 0 {
			args = append(args, "-ac", fmt.Sprint(channels))
		}
		cmd := exec.CommandContext(cctx, ff, append(args, wav)...)
		HideWindow(cmd)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", nil, fmt.Errorf("cannot read this audio file: %s", strings.TrimSpace(string(out)))
		}
		conv, err := os.ReadFile(wav)
		if err != nil {
			return "", nil, err
		}
		if pcm, err = DecodeWAV(conv); err != nil {
			return "", nil, err
		}
	}
	if pcm.Seconds() < 0.1 {
		return "", nil, errors.New("the recording is empty")
	}
	pcm = pcm.Convert(rate, channels)
	path := filepath.Join(dir, "input.wav")
	if err := os.WriteFile(path, EncodeWAV(pcm), 0o644); err != nil {
		return "", nil, err
	}
	return path, pcm, nil
}

// ── speech edit markup ───────────────────────────────────────────────────────

// EditMarkup describes how target differs from source, word by word, in the
// markup DotTTS-Edit reads: unchanged words as they are, replacements as
// <sub targ="new">old</sub>, deletions as <del>old</del>, insertions as
// <ins>new</ins>.
func EditMarkup(source, target string) (string, error) {
	a, b := strings.Fields(source), strings.Fields(target)
	if len(a) == 0 {
		return "", errors.New("the transcript of the recording is empty")
	}
	if len(b) == 0 {
		return "", errors.New("keep at least one word")
	}
	if strings.Join(a, " ") == strings.Join(b, " ") {
		return "", errors.New("change at least one word")
	}
	for _, w := range append(append([]string{}, a...), b...) {
		if strings.ContainsAny(w, "<>") {
			return "", errors.New("the text cannot contain < or >")
		}
	}
	// Longest common subsequence on words (case-insensitive).
	n, m := len(a), len(b)
	l := make([][]int, n+1)
	for i := range l {
		l[i] = make([]int, m+1)
	}
	eq := func(x, y string) bool { return strings.EqualFold(x, y) }
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if eq(a[i], b[j]) {
				l[i][j] = l[i+1][j+1] + 1
			} else {
				l[i][j] = max(l[i+1][j], l[i][j+1])
			}
		}
	}
	var parts []string
	var del, ins []string
	flush := func() {
		switch {
		case len(del) > 0 && len(ins) > 0:
			parts = append(parts, `<sub targ="`+html.EscapeString(strings.Join(ins, " "))+`">`+strings.Join(del, " ")+`</sub>`)
		case len(del) > 0:
			parts = append(parts, "<del>"+strings.Join(del, " ")+"</del>")
		case len(ins) > 0:
			parts = append(parts, "<ins>"+strings.Join(ins, " ")+"</ins>")
		}
		del, ins = nil, nil
	}
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && eq(a[i], b[j]):
			flush()
			parts = append(parts, a[i])
			i++
			j++
		case j < m && (i == n || l[i][j+1] >= l[i+1][j]):
			ins = append(ins, b[j])
			j++
		default:
			del = append(del, a[i])
			i++
		}
	}
	flush()
	return strings.Join(parts, " "), nil
}
