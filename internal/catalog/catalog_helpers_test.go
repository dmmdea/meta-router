package catalog

import "testing"

func TestEmbedText(t *testing.T) {
	s := Skill{Name: "gstack-qa", Description: "QA a web app", WhenToUse: "before shipping"}
	if got, want := s.EmbedText(), "gstack-qa QA a web app before shipping"; got != want {
		t.Fatalf("EmbedText()=%q want %q", got, want)
	}
}

func TestDedupByID(t *testing.T) {
	in := []Skill{
		{ID: "skills:a", Name: "a"},
		{ID: "skills:b", Name: "b"},
		{ID: "skills:a", Name: "a-dup"},
	}
	got := DedupByID(in)
	if len(got) != 2 {
		t.Fatalf("len=%d want 2", len(got))
	}
	if got[0].ID != "skills:a" || got[1].ID != "skills:b" {
		t.Fatalf("order/ids wrong: %+v", got)
	}
	if got[0].Name != "a" {
		t.Fatalf("kept second occurrence, want first: %q", got[0].Name)
	}
}

func TestDedupByID_Empty(t *testing.T) {
	if got := DedupByID(nil); len(got) != 0 {
		t.Fatalf("want empty, got %d", len(got))
	}
}

func TestDedupByID_NoDups(t *testing.T) {
	in := []Skill{{ID: "skills:alpha"}, {ID: "skills:beta"}, {ID: "skills:gamma"}}
	got := DedupByID(in)
	if len(got) != 3 {
		t.Fatalf("want 3, got %d", len(got))
	}
	for i := range in {
		if got[i].ID != in[i].ID {
			t.Errorf("pos %d: want %q got %q", i, in[i].ID, got[i].ID)
		}
	}
}
