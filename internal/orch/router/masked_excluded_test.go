package router

import "testing"

// masked() is a DENYLIST: an unregistered state name silently reads as
// selectable (the GLM retirement lesson). delegate-mode's "excluded" state
// must be listed, and this pins it.
func TestMaskedRegistersExcluded(t *testing.T) {
	if !masked("excluded") {
		t.Fatal(`masked("excluded") = false: an excluded lane would still be selectable`)
	}
}
