package runtime

import (
	"math"
	"testing"
)

func TestEditMarkup(t *testing.T) {
	cases := []struct{ src, dst, want string }{
		{"Ciao questa è la voce italiana di Vortelio", "Ciao questa è la voce francese di Vortelio",
			`Ciao questa è la voce <sub targ="francese">italiana</sub> di Vortelio`},
		{"one two three four", "one three four", "one <del>two</del> three four"},
		{"one two three", "one two and a half three", "one two <ins>and a half</ins> three"},
		{"the old cat sat", "the new dog sat", `the <sub targ="new dog">old cat</sub> sat`},
		{"Hello World", "hello world again", "Hello World <ins>again</ins>"},
	}
	for _, c := range cases {
		got, err := EditMarkup(c.src, c.dst)
		if err != nil || got != c.want {
			t.Errorf("EditMarkup(%q,%q)\n got %q (%v)\nwant %q", c.src, c.dst, got, err, c.want)
		}
	}
	if _, err := EditMarkup("same words", "same  words"); err == nil {
		t.Error("no change must be refused")
	}
	if _, err := EditMarkup("a b", "a <b>"); err == nil {
		t.Error("tags in text must be refused")
	}
}

func TestWAVRoundTripAndResample(t *testing.T) {
	rate := 24000
	src := &PCM{Rate: rate, Channels: 1}
	for i := 0; i < rate; i++ { // 1 s of 440 Hz
		src.Samples = append(src.Samples, float32(0.5*math.Sin(2*math.Pi*440*float64(i)/float64(rate))))
	}
	got, err := DecodeWAV(EncodeWAV(src))
	if err != nil || got.Rate != rate || got.Channels != 1 || len(got.Samples) != rate {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	st := got.Convert(44100, 2)
	if st.Rate != 44100 || st.Channels != 2 || math.Abs(st.Seconds()-1) > 0.001 {
		t.Fatalf("convert: rate %d ch %d %.3fs", st.Rate, st.Channels, st.Seconds())
	}
	if st.Samples[2*100] != st.Samples[2*100+1] {
		t.Fatal("mono→stereo must duplicate the channel")
	}
	mono := st.Convert(16000, 1)
	if mono.Channels != 1 || math.Abs(mono.Seconds()-1) > 0.001 {
		t.Fatalf("downmix: %+v", mono.Seconds())
	}
	if _, err := DecodeWAV([]byte("ID3 not a wav")); err == nil {
		t.Fatal("mp3 must not decode as WAV")
	}
}
