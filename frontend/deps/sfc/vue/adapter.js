// adapter.js — the bridge between Go-side compile.go and the real
// @vue/compiler-sfc package fetched from esm.sh.
//
// At bootstrap time esbuild bundles THIS file + @vue/compiler-sfc
// (plus the transitive deps esm.sh stitches in) into one IIFE. The
// IIFE runs once when the Goja runtime loads the bundle, installing
// globalThis.__nexus_compileSFC. compile.go then invokes the global
// for every .vue file the bundler sees.
//
// The function returns:
//
//	{ code: string, errors: [{message, line?, column?}] }
//
// — same contract compile.go expects. compile.go does NOT depend on
// any Vue-specific shape; we keep the boundary narrow on purpose so
// future SFC dialects (Svelte? legacy Vue 2?) plug in via parallel
// adapters without touching the Go side.
//
// What the assembled module exports:
//
//   - default: the SFC object with .render (from compileTemplate)
//     and component options (from compileScript)
//   - __file: source filename, threaded for dev DX
//
// Limitations (v0.1):
//   - No source maps in the synthesized module. compileTemplate
//     emits a map but we drop it; bundler-level source maps still
//     get you to the synthesized JS line.
//   - <style scoped> blocks are inlined as a one-shot
//     document.head.appendChild — no CSS extraction, no HMR. Good
//     enough for SSR + island bootstrap; fancy CSS pipelines wait.
//   - <script setup> goes through compileScript with inlineTemplate
//     — Vue's recommended path. Plain <script> + <template> works
//     too via the separate compileTemplate branch.

import * as compiler from "@vue/compiler-sfc";

(function () {
    // Stable scope id from a filename string. Avoids needing a hash
    // function in the runtime — Vue only requires the id be unique
    // per component instance, not cryptographically opaque.
    function scopeId(filename) {
        var h = 0;
        for (var i = 0; i < filename.length; i++) {
            h = ((h << 5) - h + filename.charCodeAt(i)) | 0;
        }
        return "data-v-" + (h >>> 0).toString(36);
    }

    function safeMessage(e) {
        if (!e) return "unknown error";
        if (typeof e === "string") return e;
        return e.message || String(e);
    }

    function locOf(e) {
        var loc = e && e.loc && e.loc.start;
        return {
            line: loc ? loc.line : 0,
            column: loc ? loc.column : 0,
        };
    }

    globalThis.__nexus_compileSFC = function (source, filename) {
        try {
            var parsed = compiler.parse(source, { filename: filename });
            var descriptor = parsed.descriptor;
            if (parsed.errors && parsed.errors.length) {
                return {
                    code: "",
                    errors: parsed.errors.map(function (e) {
                        var l = locOf(e);
                        return { message: safeMessage(e), line: l.line, column: l.column };
                    }),
                };
            }

            var id = scopeId(filename);
            var hasScoped = descriptor.styles.some(function (s) { return s.scoped; });
            var allErrors = [];
            var assembled = "";

            // --- script / scriptSetup ---------------------------------------
            // compileScript handles both classic <script> and <script setup>,
            // inlining the template when scriptSetup is present (Vue's
            // recommended path — produces fewer indirections than the
            // template-as-render-fn branch).
            var scriptResult = null;
            if (descriptor.script || descriptor.scriptSetup) {
                try {
                    scriptResult = compiler.compileScript(descriptor, {
                        id: id,
                        inlineTemplate: !!descriptor.scriptSetup,
                        templateOptions: {
                            id: id,
                            scoped: hasScoped,
                        },
                    });
                    // compileScript emits an ESM. We rewrite the
                    // "export default" into an assignment so the
                    // module assembly below can attach render + __file.
                    var content = scriptResult.content;
                    content = content.replace(/export\s+default\s+/, "const __sfc__ = ");
                    assembled += content + "\n";
                } catch (e) {
                    var l = locOf(e);
                    allErrors.push({ message: safeMessage(e), line: l.line, column: l.column });
                }
            } else {
                // Pure-template SFC: no script block at all.
                assembled += "const __sfc__ = {};\n";
            }

            // --- template (only when scriptSetup didn't inline it) ----------
            if (descriptor.template && !descriptor.scriptSetup) {
                try {
                    var tplResult = compiler.compileTemplate({
                        source: descriptor.template.content,
                        filename: filename,
                        id: id,
                        scoped: hasScoped,
                        compilerOptions: {
                            // Pre-stringify static parts so the synthesized
                            // module is small + avoids re-creating vnodes
                            // every render.
                            hoistStatic: true,
                        },
                    });
                    if (tplResult.errors && tplResult.errors.length) {
                        for (var i = 0; i < tplResult.errors.length; i++) {
                            var l2 = locOf(tplResult.errors[i]);
                            allErrors.push({
                                message: safeMessage(tplResult.errors[i]),
                                line: l2.line, column: l2.column,
                            });
                        }
                    }
                    assembled += tplResult.code + "\n";
                    assembled += "__sfc__.render = render;\n";
                } catch (e) {
                    var l3 = locOf(e);
                    allErrors.push({ message: safeMessage(e), line: l3.line, column: l3.column });
                }
            }

            // --- styles -----------------------------------------------------
            // For v0.1 we inline scoped styles as a side-effect script that
            // appends one <style> per component. CSS extraction + a real
            // sidecar bundle is a v0.2 concern.
            var cssChunks = [];
            for (var k = 0; k < descriptor.styles.length; k++) {
                var st = descriptor.styles[k];
                try {
                    var styleResult = compiler.compileStyle({
                        source: st.content,
                        filename: filename,
                        id: id,
                        scoped: !!st.scoped,
                    });
                    if (styleResult.errors && styleResult.errors.length) {
                        for (var j = 0; j < styleResult.errors.length; j++) {
                            allErrors.push({ message: safeMessage(styleResult.errors[j]), line: 0, column: 0 });
                        }
                    }
                    cssChunks.push(styleResult.code);
                } catch (e) {
                    allErrors.push({ message: safeMessage(e), line: 0, column: 0 });
                }
            }
            if (cssChunks.length) {
                var css = cssChunks.join("\n");
                assembled =
                    "const __css = " + JSON.stringify(css) + ";\n" +
                    "if (typeof document !== 'undefined') {\n" +
                    "  const __s = document.createElement('style');\n" +
                    "  __s.setAttribute('data-nl-sfc', " + JSON.stringify(id) + ");\n" +
                    "  __s.textContent = __css;\n" +
                    "  document.head.appendChild(__s);\n" +
                    "}\n" + assembled;
            }

            // --- finalize ---------------------------------------------------
            assembled +=
                "__sfc__.__file = " + JSON.stringify(filename) + ";\n" +
                "__sfc__.__scopeId = " + JSON.stringify(id) + ";\n" +
                "export default __sfc__;\n";

            return { code: assembled, errors: allErrors };
        } catch (e) {
            return {
                code: "",
                errors: [{ message: "adapter crashed: " + safeMessage(e), line: 0, column: 0 }],
            };
        }
    };
})();
