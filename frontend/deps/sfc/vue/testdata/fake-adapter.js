// fake-adapter.js — minimal stand-in for the @vue/compiler-sfc
// adapter the production code loads.
//
// Used only by package vue's tests. Proves the Go ↔ Goja plumbing
// (bundle load, function bind, arg marshal, result decode) is
// correct without pulling the real Vue compiler into the test
// dependency graph.
//
// Contract matches what compile.go expects:
//
//   globalThis.__nexus_compileSFC(source, filename)
//     → { code: string, errors?: [{message, line?, column?}] }
//
// This fake's behavior is deterministic so tests can assert exact
// output bytes:
//
//   - Strips out the <template>...</template> body and emits it as
//     a JS string default-export.
//   - When source starts with the literal "BOOM" (test-only
//     sentinel), returns one error to exercise the errors path.

(function () {
    globalThis.__nexus_compileSFC = function (source, filename) {
        if (source.startsWith("BOOM")) {
            return {
                code: "",
                errors: [
                    { message: "synthetic test error", line: 1, column: 1 },
                ],
            };
        }

        // Cheap template extraction — good enough for the fake.
        var m = source.match(/<template>([\s\S]*?)<\/template>/);
        var tmpl = m ? m[1].trim() : "";

        return {
            code:
                "export default { template: " +
                JSON.stringify(tmpl) +
                ", __filename: " +
                JSON.stringify(filename) +
                " };",
        };
    };
})();
