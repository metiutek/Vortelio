package hub

import "testing"

func files(names ...string) []hfFileInfo {
	var out []hfFileInfo
	for i, n := range names {
		out = append(out, hfFileInfo{Rfilename: n, Size: int64(100 + i)})
	}
	return out
}

func TestPlanGGUFShardsAndMmproj(t *testing.T) {
	fs := files(
		"BF16/Model-BF16-00002-of-00003.gguf",
		"BF16/Model-BF16-00001-of-00003.gguf",
		"BF16/Model-BF16-00003-of-00003.gguf",
		"Model-Q4_K_M.gguf",
		"mmproj-BF16.gguf",
		"mmproj-F16.gguf",
		"mmproj-F32.gguf",
	)
	chosen, err := pickFile(fs, "BF16")
	if err != nil || chosen.Rfilename != "BF16/Model-BF16-00001-of-00003.gguf" {
		t.Fatalf("hint BF16 picked %q (%v)", chosen.Rfilename, err)
	}
	plan := planGGUF(fs, chosen, true)
	if len(plan.Shards) != 3 || plan.Shards[0].Rfilename != "BF16/Model-BF16-00001-of-00003.gguf" || plan.Shards[2].Rfilename != "BF16/Model-BF16-00003-of-00003.gguf" {
		t.Fatalf("shards: %+v", plan.Shards)
	}
	if plan.Mmproj == nil || plan.Mmproj.Rfilename != "mmproj-F16.gguf" {
		t.Fatalf("mmproj: %+v", plan.Mmproj)
	}
}

func TestPickFileDefaultsAndProjector(t *testing.T) {
	fs := files("mmproj-F16.gguf", "M-IQ1_S.gguf", "M-Q4_K_M.gguf", "M-Q8_0.gguf")
	f, err := pickFile(fs, "")
	if err != nil || f.Rfilename != "M-Q4_K_M.gguf" {
		t.Fatalf("default picked %q", f.Rfilename)
	}
	// "F16" also matches the projector name, which must not be chosen as weights.
	fs = files("mmproj-F16.gguf", "M-F16.gguf")
	f, _ = pickFile(fs, "F16")
	if f.Rfilename != "M-F16.gguf" {
		t.Fatalf("hint F16 picked %q", f.Rfilename)
	}
	p := planGGUF(fs, f, false)
	if p.Mmproj != nil {
		t.Fatal("non-llm plan must not add a projector")
	}
}
