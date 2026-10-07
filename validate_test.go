package nexus

import "testing"

func TestValidateListOfStructs(t *testing.T) {
	type line struct {
		SKU string `json:"sku" validate:"required"`
	}
	type order struct {
		Lines []line `json:"lines"`
	}
	err := Validate(order{Lines: []line{{SKU: "a"}, {}, {SKU: "c"}, {}}})
	e := ErrorOf(err)
	if e == nil || len(e.Fields["lines[1].sku"]) == 0 || len(e.Fields["lines[3].sku"]) == 0 || len(e.Fields) != 2 {
		t.Fatalf("errors: %+v", e)
	}
}
