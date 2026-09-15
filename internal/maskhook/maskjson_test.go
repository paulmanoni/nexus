package maskhook

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func installTestPolicy(t *testing.T) {
	t.Helper()
	Install(Hooks{
		IsID: func(key string) bool {
			lk := strings.ToLower(key)
			return lk == "id" || lk == "ids" || strings.HasSuffix(key, "Id") || strings.HasSuffix(key, "Ids")
		},
		Mask:   func(_ string, n int64) (string, bool) { return "M" + strconv.FormatInt(n, 10), true },
		Unmask: func(_ string, s string) (int64, bool) { return 0, false },
		Skip:   func(key string) bool { return key == "categories" },
	})
	t.Cleanup(Uninstall)
}

// TestMaskJSONBytes_MatchesTreePipeline is the differential test: for
// every fixture, the single-pass byte rewriter must produce output
// semantically identical to the original toTree+walk pipeline
// (compared decoded — the tree re-encode sorts keys alphabetically,
// the byte pass preserves struct order).
func TestMaskJSONBytes_MatchesTreePipeline(t *testing.T) {
	installTestPolicy(t)

	fixtures := []any{
		map[string]any{"id": 41, "ownerId": 7, "name": "a"},
		map[string]any{"ids": []int{1, 2, 3}, "total": 3},
		map[string]any{"nested": map[string]any{"id": 9, "deep": map[string]any{"parentId": 12}}},
		map[string]any{"rowIds": [][]int64{{1, 2}, {3}}}, // arrays inherit the key through depth
		map[string]any{"categories": []map[string]any{{"id": 5, "name": "x"}}, "id": 6}, // Skip prunes subtree
		map[string]any{"id": -12, "big": map[string]any{"id": int64(1) << 60}},
		map[string]any{"price": 1.5, "ratio": 2.0, "id": 3, "flag": true, "none": nil}, // floats never mask
		map[string]any{"id": "already-a-string", "list": []any{"a", 1.25, map[string]any{"id": 8}}},
		map[string]any{"msg": "quotes \" and \\ and <html> & unicode ✓", "id": 1},
		map[string]any{"weird\"key": 1, "id": 2}, // escaped key
		[]map[string]any{{"id": 1}, {"id": 2}},   // top-level array
		map[string]any{},
	}

	for i, v := range fixtures {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("fixture %d: marshal: %v", i, err)
		}

		// Old pipeline: decode to tree, walk, re-encode.
		tree, err := decode(raw)
		if err != nil {
			t.Fatalf("fixture %d: decode: %v", i, err)
		}
		masked := walk(tree, "", maskLeaf)
		wantRaw, err := json.Marshal(masked)
		if err != nil {
			t.Fatalf("fixture %d: re-marshal: %v", i, err)
		}

		gotRaw := maskJSONBytes(raw)

		var want, got any
		if err := json.Unmarshal(wantRaw, &want); err != nil {
			t.Fatalf("fixture %d: want undecodable: %v\n%s", i, err, wantRaw)
		}
		if err := json.Unmarshal(gotRaw, &got); err != nil {
			t.Fatalf("fixture %d: got undecodable: %v\n%s", i, err, gotRaw)
		}
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("fixture %d diverges\n tree: %s\n byte: %s", i, wantRaw, gotRaw)
		}
	}
}

// TestMaskResponse_Basics pins the entry-point contract.
func TestMaskResponse_Basics(t *testing.T) {
	if _, ok := MaskResponse(map[string]any{"id": 1}); ok {
		t.Fatal("masking disabled: must report ok=false")
	}
	installTestPolicy(t)
	type User struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	b, ok := MaskResponse(User{ID: 41, Name: "a"})
	if !ok {
		t.Fatal("want ok")
	}
	if string(b) != `{"id":"M41","name":"a"}` {
		t.Fatalf("got %s", b)
	}
	// Struct field order is preserved (the tree pipeline re-sorted).
	type Ordered struct {
		Z  int `json:"z"`
		ID int `json:"id"`
		A  int `json:"a"`
	}
	b, _ = MaskResponse(Ordered{Z: 1, ID: 2, A: 3})
	if string(b) != `{"z":1,"id":"M2","a":3}` {
		t.Fatalf("field order not preserved: %s", b)
	}
}

// BenchmarkMaskPipelines compares the retired tree pipeline against
// the single-pass byte rewriter on a realistic list response.
func BenchmarkMaskPipelines(b *testing.B) {
	Install(Hooks{
		IsID: func(key string) bool {
			return key == "id" || strings.HasSuffix(key, "Id")
		},
		Mask:   func(_ string, n int64) (string, bool) { return "MMMMMMMMMMMMMMMMMMMM" + strconv.FormatInt(n, 10), true },
		Unmask: func(_ string, s string) (int64, bool) { return 0, false },
	})
	b.Cleanup(Uninstall)

	type Row struct {
		ID      int64   `json:"id"`
		OwnerID int64   `json:"ownerId"`
		Name    string  `json:"name"`
		Price   float64 `json:"price"`
		Active  bool    `json:"active"`
	}
	rows := make([]Row, 50)
	for i := range rows {
		rows[i] = Row{ID: int64(i + 1), OwnerID: int64(i + 100), Name: "row name here", Price: 12.5, Active: true}
	}
	resp := map[string]any{"items": rows, "total": 50}

	b.Run("tree", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			masked := MaskValue(resp)
			if _, err := json.Marshal(masked); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("bytes", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, ok := MaskResponse(resp); !ok {
				b.Fatal("not masked")
			}
		}
	})
}
