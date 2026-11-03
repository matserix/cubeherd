package web

import (
	"testing"

	"cubeherd/internal/access"
)

func TestPermissionsAreDescribed(t *testing.T) {
	for _, i := range access.Catalog {
		if txt := permText[i.ID]; txt[0] == "" || txt[1] == "" {
			t.Errorf("permission %s has no name or description in permText", i.ID)
		}
		if groupText[i.Group] == "" {
			t.Errorf("permission group %s has no label", i.Group)
		}
	}
}
