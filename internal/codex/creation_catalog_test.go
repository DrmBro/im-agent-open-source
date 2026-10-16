package codex

import "testing"

func TestCreationSelectionUsesCatalogCapabilities(t *testing.T) {
	c := CreationCatalog{Projects: []ProjectChoice{{Value: "p:0", ID: "p", Path: "/work"}}, Models: []ModelChoice{{Model: "model", Efforts: []string{"high"}}}}
	s := CreationSelection{Project: "p:0", Model: "model", Effort: "high", Prompt: "question"}
	if _, e := c.Validate(s); e != nil {
		t.Fatal(e)
	}
	for _, invalid := range []CreationSelection{{Project: "/etc", Model: "model", Effort: "high"}, {Project: "p:0", Model: "other", Effort: "high"}, {Project: "p:0", Model: "model", Effort: "unsupported"}} {
		if _, e := c.Validate(invalid); e == nil {
			t.Fatal("unlisted choice accepted")
		}
	}
}
