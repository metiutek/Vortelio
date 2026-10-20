package hub

import (
	"reflect"
	"testing"
)

func TestTasksFromSpec(t *testing.T) {
	var s audioSpec
	s.Family, s.Tasks = "dots_tts", []string{"tts", "clone"}
	if got := tasksFromSpec(s, "dots-tts-edit-q8_0.gguf"); !reflect.DeepEqual(got, []string{"clone", "edit"}) {
		t.Fatalf("dots edit: %v", got)
	}
	s.Family, s.Tasks = "kokoro_tts", []string{"tts"}
	s.UI.BuiltinVoices = []string{"af_heart"}
	if got := tasksFromSpec(s, "kokoro.gguf"); !reflect.DeepEqual(got, []string{"tts"}) {
		t.Fatalf("kokoro: %v", got)
	}
	s = audioSpec{Family: "stable_audio", Tasks: []string{"music", "sfx", "edit"}}
	if got := tasksFromSpec(s, "x.gguf"); !reflect.DeepEqual(got, []string{"music", "sfx"}) {
		t.Fatalf("stable audio: %v", got)
	}
}

func TestAudioCatalogRegistered(t *testing.T) {
	for _, e := range AudioCatalog {
		ref, err := ParseModelRef(e.Ref)
		if err != nil {
			t.Fatalf("%s: %v", e.Ref, err)
		}
		if _, ok := hfRegistry[ref.Type+"/"+ref.Name+":"+ref.Tag]; !ok {
			t.Errorf("%s not in registry", e.Ref)
		}
		if got, ok := AudioCatalogFor(ref); !ok || got.Ref != e.Ref {
			t.Errorf("AudioCatalogFor(%s) = %v", e.Ref, ok)
		}
	}
}
