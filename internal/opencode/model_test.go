package opencode

import "testing"

func TestConfiguredModelReferenceParsing(t *testing.T) {
	for _, raw := range []string{"missing-provider", "/model", "provider/", "provider/model#", "provider/model#a#b", "provider/model with space", "provider/model\n"} {
		if _, err := ParseModelReference(raw); err == nil {
			t.Fatal("invalid model reference accepted", raw)
		}
	}
	model, err := ParseModelReference("configured/model/name#high")
	if err != nil || model.ProviderID != "configured" || model.ID != "model/name" || model.Variant != "high" {
		t.Fatal("configured model reference lost identity", err)
	}
	if model, err = ParseModelReference(""); err != nil || model != nil {
		t.Fatal("empty reference must retain native default")
	}
}
