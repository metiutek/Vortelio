package runtime

import (
	"testing"
)

func TestSamplingProgressParsing(t *testing.T) {
	tests := []struct {
		line        string
		wantCur     int
		wantTotal   int
		wantNoMatch bool
	}{
		{line: "  |=========>      | 2/4 - 5.59s/it", wantCur: 2, wantTotal: 4},
		{line: "|==================| 20/20 - 1.20it/s", wantCur: 20, wantTotal: 20},
		// Tensor loading bars use MB/s and must not be read as sampling steps.
		{line: "  |##################| 196/196 - 801.09MB/s", wantNoMatch: true},
		{line: "[INFO ] stable-diffusion.cpp:5592 - generate_image 256x256", wantNoMatch: true},
	}
	for _, tt := range tests {
		m := sdSamplingRe.FindStringSubmatch(tt.line)
		if tt.wantNoMatch {
			if m != nil {
				t.Errorf("line %q matched but should not", tt.line)
			}
			continue
		}
		if m == nil {
			t.Errorf("line %q did not match", tt.line)
			continue
		}
		if m[1] != itoa(tt.wantCur) || m[2] != itoa(tt.wantTotal) {
			t.Errorf("line %q → %s/%s, want %d/%d", tt.line, m[1], m[2], tt.wantCur, tt.wantTotal)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
