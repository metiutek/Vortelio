package runtime

import "testing"

func TestParseLlamaCaps(t *testing.T) {
	help := `-fa,   --flash-attn [on|off|auto]       set Flash Attention use
-fit,  --fit [on|off]                   whether to adjust unset arguments
-fitc, --fit-ctx N                      minimum ctx size
--jinja, --no-jinja                     whether to use jinja
--embedding, --embeddings               restrict to only support embedding use case
--mmproj-auto, --no-mmproj, --no-mmproj-auto`
	c := parseLlamaCaps(help)
	if !c.Fit || !c.FitCtx || !c.Jinja || !c.Embeddings || !c.FlashAttnValue || !c.NoMmprojAuto {
		t.Fatalf("%+v", c)
	}
	old := parseLlamaCaps("-fa, --flash-attn  enable flash attention\n-ngl N")
	if old.Fit || old.FlashAttnValue || old.Jinja {
		t.Fatalf("old build misread: %+v", old)
	}
}
