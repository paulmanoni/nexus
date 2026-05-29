//go:build cgo && network

// Side-by-side benchmark of the two SFC compiler backends against the
// real @vue/compiler-sfc bundle:
//
//   - *Compiler     — CGo binding to native QuickJS (compile.go)
//   - *QJSCompiler  — CGo-free QuickJS-NG via Wazero WASM (compile_qjs.go)
//
// Both run the identical bundle Bootstrap produces, so this isolates
// the engine cost. Network-gated (Bootstrap pulls the compiler from
// esm.sh on a cold cache) and needs cgo for the native backend:
//
//	CGO_ENABLED=1 go test -tags "cgo network" \
//	  -run TestQJS -bench 'Compile|Construct' -benchmem \
//	  ./frontend/deps/sfc/vue
//
// The bundle is bootstrapped once per process into a persistent temp
// dir, so a second run is offline-fast.

package vue

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/frontend/deps/fetcher"
	"github.com/paulmanoni/nexus/frontend/deps/store"
)

const benchVersion = "@vue/compiler-sfc@" + DefaultCompilerVersion

// benchSFC is a representative Vue 3 SFC: script setup with a couple
// of refs + a computed, a non-trivial template with directives, and a
// scoped style block — the kind of file that dominates a real app.
const benchSFC = `<template>
  <section class="card">
    <h1 class="title">{{ title }}</h1>
    <ul>
      <li v-for="(item, i) in items" :key="i" @click="select(item)">
        {{ i + 1 }}. {{ item.label }}
      </li>
    </ul>
    <button :disabled="!canSubmit" @click="submit">Save ({{ count }})</button>
  </section>
</template>

<script setup lang="ts">
import { ref, computed } from 'vue'

const title = ref('Items')
const count = ref(0)
const items = ref<{ label: string }[]>([{ label: 'a' }, { label: 'b' }])
const canSubmit = computed(() => count.value < 10)

function select(item: { label: string }) {
  count.value++
  title.value = item.label
}
function submit() {
  count.value = 0
}
</script>

<style scoped>
.card { padding: 16px; border: 1px solid #e5e7eb; border-radius: 8px; }
.title { color: rebeccapurple; font-weight: 700; }
</style>
`

var (
	benchBundleOnce sync.Once
	benchBundleData []byte
	benchBundleErr  error
)

// loadBenchBundle bootstraps the real compiler bundle once per
// process. Uses a persistent temp dir so reruns hit the cache and
// don't re-fetch from esm.sh. Skips (not fails) when the bundle can't
// be obtained — e.g. offline with a cold cache.
func loadBenchBundle(tb testing.TB) []byte {
	tb.Helper()
	benchBundleOnce.Do(func() {
		dir := filepath.Join(os.TempDir(), "nexus-vue-bench")
		s, err := store.New(filepath.Join(dir, "cache"))
		if err != nil {
			benchBundleErr = err
			return
		}
		f := fetcher.New(s, "")
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()
		benchBundleData, benchBundleErr = Bootstrap(ctx, BootstrapOptions{
			Store:     s,
			Fetcher:   f,
			BundleDir: filepath.Join(dir, "bundles"),
		})
	})
	if benchBundleErr != nil {
		tb.Skipf("vue bootstrap unavailable (offline?): %v", benchBundleErr)
	}
	return benchBundleData
}

// TestQJS_RealSFCParity proves the WASM backend produces the same
// output as the CGo backend for the same bundle + input. Identical
// bytes are expected: both run the same deterministic compiler over
// the same source.
func TestQJS_RealSFCParity(t *testing.T) {
	bundle := loadBenchBundle(t)

	cc, err := NewCompiler(bundle, benchVersion)
	if err != nil {
		t.Fatalf("NewCompiler (cgo): %v", err)
	}
	defer cc.Close()
	qc, err := NewQJSCompiler(bundle, benchVersion)
	if err != nil {
		t.Fatalf("NewQJSCompiler (wasm): %v", err)
	}
	defer qc.Close()

	cgoRes, err := cc.Compile(benchSFC, "Card.vue")
	if err != nil {
		t.Fatalf("cgo Compile: %v", err)
	}
	qjsRes, err := qc.Compile(benchSFC, "Card.vue")
	if err != nil {
		t.Fatalf("wasm Compile: %v", err)
	}

	if len(cgoRes.Errors) != 0 || len(qjsRes.Errors) != 0 {
		t.Fatalf("compile errors: cgo=%+v wasm=%+v", cgoRes.Errors, qjsRes.Errors)
	}
	if cgoRes.Code != qjsRes.Code {
		t.Errorf("backends disagree:\n  cgo len=%d\n  wasm len=%d", len(cgoRes.Code), len(qjsRes.Code))
	}
	for _, want := range []string{"export default __sfc__", "data-v-", "rebeccapurple"} {
		if !contains(qjsRes.Code, want) {
			t.Errorf("wasm output missing %q", want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func BenchmarkConstruct_CGo(b *testing.B) {
	bundle := loadBenchBundle(b)
	b.ResetTimer()
	for range b.N {
		c, err := NewCompiler(bundle, benchVersion)
		if err != nil {
			b.Fatal(err)
		}
		c.Close()
	}
}

func BenchmarkConstruct_QJS(b *testing.B) {
	bundle := loadBenchBundle(b)
	b.ResetTimer()
	for range b.N {
		c, err := NewQJSCompiler(bundle, benchVersion)
		if err != nil {
			b.Fatal(err)
		}
		c.Close()
	}
}

func BenchmarkCompile_CGo(b *testing.B) {
	bundle := loadBenchBundle(b)
	c, err := NewCompiler(bundle, benchVersion)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	b.ResetTimer()
	for range b.N {
		if _, err := c.Compile(benchSFC, "Card.vue"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompile_QJS(b *testing.B) {
	bundle := loadBenchBundle(b)
	c, err := NewQJSCompiler(bundle, benchVersion)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	b.ResetTimer()
	for range b.N {
		if _, err := c.Compile(benchSFC, "Card.vue"); err != nil {
			b.Fatal(err)
		}
	}
}
