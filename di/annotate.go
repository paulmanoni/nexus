package di

// Annotate wraps a constructor or invoke with annotations, mirroring
// fx.Annotate. nexus uses two forms:
//
//	// tag a constructor's result into a value group (GraphQL fields):
//	di.Provide(di.Annotate(newField, di.ResultTags(`group:"nexus.graph.fields"`)))
//
//	// pull an invoke parameter from a group / mark it optional:
//	di.Invoke(di.Annotate(autoMount,
//	    di.ParamTags("", "", `group:"nexus.graph.fields"`)))
//	di.Invoke(di.Annotate(applyGate, di.ParamTags("", `optional:"true"`)))
//
// ResultTags and ParamTags accept one struct-tag string per result/parameter
// position (an empty string leaves that position untagged). The `group:"..."`
// and `optional:"true"` keys are interpreted; others are ignored, keeping the
// surface identical to the fx call sites being migrated.
func Annotate(target any, anns ...Annotation) any {
	a := annotatedCtor{ctor: target}
	for _, an := range anns {
		an.apply(&a)
	}
	return a
}

// annotatedCtor is the sentinel Provide/Invoke recognizes. resultGroups and
// paramGroups hold the RAW per-position tag strings (parsed at resolve time).
type annotatedCtor struct {
	ctor         any
	resultGroups []string
	paramGroups  []string
}

// Annotation configures an annotatedCtor. Implemented by ResultTags/ParamTags.
type Annotation interface{ apply(*annotatedCtor) }

type resultTags struct{ tags []string }

func (r resultTags) apply(a *annotatedCtor) { a.resultGroups = append([]string(nil), r.tags...) }

// ResultTags tags constructor results by position. A `group:"name"` tag routes
// that result into the named value group instead of registering it as a plain
// typed output.
func ResultTags(tags ...string) Annotation { return resultTags{tags: tags} }

type paramTags struct{ tags []string }

func (p paramTags) apply(a *annotatedCtor) { a.paramGroups = append([]string(nil), p.tags...) }

// ParamTags tags parameters by position. `group:"name"` makes that parameter
// (which must be a slice) resolve from the named value group; `optional:"true"`
// resolves to the zero value when no provider exists.
func ParamTags(tags ...string) Annotation { return paramTags{tags: tags} }
