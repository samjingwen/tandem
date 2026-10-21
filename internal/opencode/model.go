package opencode

import (
	"strings"
	"unicode"

	"github.com/samjingwen/tandem/core"
)

// ParseModelReference accepts the operator's configured provider/model#variant
// reference. Availability/authentication still belongs to OpenCode, not Tandem.
func ParseModelReference(raw string) (*Model, error) {
	if raw == "" {
		return nil, nil
	}
	if strings.IndexFunc(raw, unicode.IsSpace) >= 0 || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return nil, core.Invalid("model reference cannot contain whitespace/control characters")
	}
	reference, variant, hasVariant := strings.Cut(raw, "#")
	provider, model, ok := strings.Cut(reference, "/")
	if !ok || provider == "" || model == "" || (hasVariant && (variant == "" || strings.Contains(variant, "#"))) {
		return nil, core.Invalid("model must be provider/model with optional #variant")
	}
	return &Model{ProviderID: provider, ID: model, Variant: variant}, nil
}
