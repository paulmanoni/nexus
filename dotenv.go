package nexus

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/paulmanoni/nexus/di"
)

// DotenvDefaultPath is the file LoadDotenvIfPresent reads when no
// explicit path is supplied. Matches the .env convention every Go,
// Node, Python, and Ruby developer already knows from
// dotenv / direnv / docker-compose.
const DotenvDefaultPath = ".env"

// LoadDotenvIfPresent reads `./.env` (or the supplied path) and
// populates os.Environ for any key NOT already set in the process
// environment. Use it in main() so ${VAR} placeholders in nexus.toml
// — and any other code calling os.Getenv at boot — resolve from the
// file:
//
//	func main() {
//	    nexus.Run(nexus.Config{...},
//	        nexus.LoadDotenvIfPresent(),   // ← reads .env if present
//	        appModule,
//	    )
//	}
//
// Behavior:
//
//   - Missing file → silent no-op. Production runs without a .env
//     file boot normally; the platform's injected env vars are the
//     source of truth.
//   - Existing env vars are preserved. A real `DB_PASSWORD` set by
//     systemd / docker / kubectl always beats the .env entry. This
//     matches Spring's precedence (env vars beat application.yml)
//     and the dotenv convention every other ecosystem uses.
//   - Parse errors (malformed line, unterminated quotes) fail boot
//     with the line number. A broken .env should not silently
//     produce a partially-loaded environment.
//
// File format — minimal, no shell-style expansion:
//
//	KEY=value             # plain
//	KEY="value with =="   # quoted; quotes stripped
//	KEY='literal $foo'    # single-quoted; literal, no expansion
//	# comment
//	export KEY=value      # leading 'export ' allowed, ignored
//
// What's intentionally NOT supported (lean parser, predictable
// behavior):
//
//   - Variable expansion inside values (no $OTHER references)
//   - Multi-line values
//   - Shell command substitution
//
// Operators wanting those features should source the .env in a
// real shell before launching the binary — the framework's job is
// to consume what the environment already has, not to be a shell.
func LoadDotenvIfPresent(path ...string) Option {
	p := DotenvDefaultPath
	if len(path) > 0 && path[0] != "" {
		p = path[0]
	}
	if err := loadDotenvFile(p); err != nil {
		// Errors other than "file missing" should stop boot — a
		// malformed .env is an operator bug worth surfacing loudly.
		return Raw(di.Error(fmt.Errorf("nexus.LoadDotenvIfPresent: %w", err)))
	}
	return Options()
}

// MustLoadDotenv is the strict variant: a missing file fails boot
// instead of being a no-op. Use it when the .env contents are
// required (you've intentionally committed a stub for dev and want
// to catch "forgot to copy it" mistakes before they become silent
// `${VAR}` lookup failures).
func MustLoadDotenv(path ...string) Option {
	p := DotenvDefaultPath
	if len(path) > 0 && path[0] != "" {
		p = path[0]
	}
	if _, err := os.Stat(p); err != nil {
		return Raw(di.Error(fmt.Errorf("nexus.MustLoadDotenv: %s: %w", p, err)))
	}
	if err := loadDotenvFile(p); err != nil {
		return Raw(di.Error(fmt.Errorf("nexus.MustLoadDotenv: %w", err)))
	}
	return Options()
}

// loadDotenvFile reads the file and applies each KEY=value pair to
// os.Environ, skipping keys already present. Missing file is a
// silent success; any other I/O or parse problem returns an error
// the caller wraps.
func loadDotenvFile(path string) error {
	f, err := os.Open(path) // #nosec G304 -- operator-supplied path
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	applied := 0
	skipped := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // 1MB max line
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		key, value, ok, err := parseDotenvLine(scanner.Text())
		if err != nil {
			return fmt.Errorf("%s:%d: %w", path, lineNo, err)
		}
		if !ok {
			continue // blank line or comment
		}
		// Preserve any value already in the environment. Mirrors
		// dotenv's "first source wins" rule — real env vars beat
		// the file.
		if _, exists := os.LookupEnv(key); exists {
			skipped++
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("%s:%d: setenv %s: %w", path, lineNo, key, err)
		}
		applied++
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	// One-line confirmation at boot so operators can SEE the file
	// was consumed. Lots of "why isn't my env var working" hours
	// have been spent debugging silent dotenv loaders.
	log.Printf("nexus: loaded %s (%d vars applied, %d skipped — already in env)", path, applied, skipped)
	return nil
}

// parseDotenvLine returns (key, value, ok, err) for one line.
//
//	ok=false  → blank line or comment (caller skips)
//	ok=true   → key/value pair to apply
//	err != nil → malformed line; caller fails boot
//
// Accepted shapes:
//
//	KEY=value
//	KEY="value"
//	KEY='value'
//	export KEY=value          # 'export ' prefix tolerated, ignored
//	# comment                 # blank or '#'-prefixed
//
// Inline comments (` # comment` after a value) are honored ONLY
// when the value is unquoted — a `"hello # world"` keeps the # as
// part of the value because the quote pair makes the intent clear.
func parseDotenvLine(line string) (key, value string, ok bool, err error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", false, nil
	}
	// `export KEY=value` is a shell-friendly convention — accept
	// + strip the prefix so operators can paste lines copied from
	// a sourceable .envrc.
	trimmed = strings.TrimPrefix(trimmed, "export ")
	trimmed = strings.TrimSpace(trimmed)

	eq := strings.IndexByte(trimmed, '=')
	if eq <= 0 {
		return "", "", false, fmt.Errorf("missing `=` in %q", line)
	}
	key = strings.TrimSpace(trimmed[:eq])
	if !isValidEnvKey(key) {
		return "", "", false, fmt.Errorf("invalid variable name %q", key)
	}
	rest := trimmed[eq+1:]
	rest = strings.TrimLeft(rest, " \t")

	// Quoted value? Either flavour of quote — both treated as
	// "value runs to the matching closing quote, content kept
	// verbatim". We deliberately don't process escape sequences
	// like \n inside double quotes; that's a shell behavior, not
	// a config-file behavior, and conflating them surprises
	// operators who didn't expect their string to mutate.
	if len(rest) >= 2 && (rest[0] == '"' || rest[0] == '\'') {
		q := rest[0]
		end := strings.IndexByte(rest[1:], q)
		if end < 0 {
			return "", "", false, fmt.Errorf("unterminated quoted value in %q", line)
		}
		value = rest[1 : 1+end]
		return key, value, true, nil
	}

	// Unquoted: trim trailing whitespace + inline `# comment`.
	if hash := strings.Index(rest, " #"); hash >= 0 {
		rest = rest[:hash]
	}
	value = strings.TrimRight(rest, " \t")
	return key, value, true, nil
}

// isValidEnvKey enforces the POSIX-ish env-var name shape so a
// malformed .env entry doesn't ride into os.Environ where it could
// confuse downstream consumers. Allowed: [A-Za-z_][A-Za-z0-9_]*.
// Mirrors what `export` accepts in bash/zsh.
func isValidEnvKey(k string) bool {
	if k == "" {
		return false
	}
	for i, c := range k {
		if i == 0 {
			if !isAlpha(c) && c != '_' {
				return false
			}
			continue
		}
		if !isAlpha(c) && !isDigit(c) && c != '_' {
			return false
		}
	}
	return true
}

func isAlpha(c rune) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isDigit(c rune) bool { return c >= '0' && c <= '9' }
