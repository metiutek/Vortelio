package cloud

import "testing"

func TestOllamaChoicesOrderAndLabels(t *testing.T) {
	got := ollamaChoicesFor([]string{"kimi-k3", "brand-new-model", "gpt-oss:20b", "gemma4:31b"})
	want := []string{"gpt-oss:20b", "gemma4:31b", "kimi-k3", "brand-new-model"}
	for i, id := range want {
		if got[i][0] != id {
			t.Fatalf("pos %d = %s, want %s (all: %v)", i, got[i][0], id, got)
		}
	}
	if got[0][1] != "gpt-oss 20B · $0.07/$0.3 per 1M" {
		t.Fatalf("label = %q", got[0][1])
	}
	if got[3][1] != "brand-new-model" {
		t.Fatalf("unpriced label = %q", got[3][1])
	}
}

func TestOllamaLiveCatalog(t *testing.T) {
	if testing.Short() {
		t.Skip("network")
	}
	c := Choices("ollamacloud")
	if len(c) == 0 {
		t.Fatal("no choices")
	}
	t.Logf("%d models, first: %v", len(c), c[0])
}
