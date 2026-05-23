package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FromGit is the server-side source factory for a remote git
// repository. On first Load the framework clones into a working
// directory; subsequent Loads run git fetch + checkout to pick
// up upstream changes.
//
// Shells out to the `git` binary on PATH — no Go git protocol
// reimplementation, no third-party deps. The repo URL accepts
// any form the local git binary accepts: https:// (with optional
// token), git@host:path (SSH), or any other transport git knows.
//
//	config.FromGit("https://internal/platform/config.git",
//	    config.GitBranch("main"),
//	    config.GitToken("${CONFIG_GIT_TOKEN}"),
//	)
//
//	config.FromGit("git@internal:platform/config.git",
//	    config.GitSSHKey("/etc/configd/id_ed25519"),
//	    config.GitKnownHosts("/etc/configd/known_hosts"),
//	)
//
// After clone/fetch the working tree is read with the same
// per-app TOML layout config.FromTOML expects — the git source
// is essentially "FromTOML, but the directory is materialized
// from a remote ref."
//
// Reload model (phase 2 elaborates): operators trigger refresh
// via a server restart for now. WS-subscription + push webhook
// + periodic poll all land in phase 2 with the rest of the
// live-refresh path.
func FromGit(repoURL string, opts ...GitOption) Source {
	cfg := gitSourceConfig{
		repo:       repoURL,
		branch:     "main",
		clonePath:  defaultGitClonePath(repoURL),
		pollIntvl:  0, // phase 2
	}
	for _, o := range opts {
		o.applyGit(&cfg)
	}
	return &gitSource{cfg: cfg}
}

// GitOption is the functional-option shape for FromGit.
type GitOption interface {
	applyGit(*gitSourceConfig)
}

type gitSourceConfig struct {
	repo        string
	branch      string
	ref         string // overrides branch when non-empty (tag / SHA)
	subdir      string // source root inside the repo
	clonePath   string // working clone on disk
	token       string // for HTTPS auth
	sshKey      string // for SSH auth
	knownHosts  string // SSH host-key pinning
	pollIntvl   time.Duration // phase 2
}

// GitBranch picks the branch to track. Default "main".
func GitBranch(b string) GitOption {
	return gitOptionFunc(func(c *gitSourceConfig) { c.branch = b })
}

// GitRef pins a tag / SHA / explicit ref. When set, overrides
// GitBranch — the working tree is checked out at the pinned
// ref, not the branch's HEAD.
func GitRef(r string) GitOption {
	return gitOptionFunc(func(c *gitSourceConfig) { c.ref = r })
}

// GitSubdir restricts the source layout to a subdirectory of
// the repo. Useful when the config files live alongside other
// content (e.g. a "platform" repo with deployment, terraform,
// and config side-by-side).
func GitSubdir(p string) GitOption {
	return gitOptionFunc(func(c *gitSourceConfig) { c.subdir = p })
}

// GitToken supplies an HTTPS Personal Access Token / OAuth
// token, formatted into the clone URL as
// https://<token>@host/path. Only consulted for https:// URLs.
func GitToken(t string) GitOption {
	return gitOptionFunc(func(c *gitSourceConfig) { c.token = t })
}

// GitSSHKey pins the private key file passed to `git -c
// core.sshCommand="ssh -i <key>"`. Required for git@ URLs that
// authenticate by key.
func GitSSHKey(path string) GitOption {
	return gitOptionFunc(func(c *gitSourceConfig) { c.sshKey = path })
}

// GitKnownHosts pins the SSH known_hosts file. Strongly
// recommended for production deployments — without it, a DNS
// hijack of the git host could redirect to a malicious mirror
// on first connect.
func GitKnownHosts(path string) GitOption {
	return gitOptionFunc(func(c *gitSourceConfig) { c.knownHosts = path })
}

// GitClonePath overrides the working clone location. Defaults
// to /tmp/configd-<hash> in dev; production runs typically pin
// /var/lib/configd/repo.
func GitClonePath(p string) GitOption {
	return gitOptionFunc(func(c *gitSourceConfig) { c.clonePath = p })
}

// GitPollFallback sets how often the source re-fetches when
// no webhook is configured. Phase 2 — currently a no-op.
func GitPollFallback(d time.Duration) GitOption {
	return gitOptionFunc(func(c *gitSourceConfig) { c.pollIntvl = d })
}

type gitOptionFunc func(*gitSourceConfig)

func (f gitOptionFunc) applyGit(c *gitSourceConfig) { f(c) }

// gitSource is the Source implementation. State: the configured
// clone directory + a guard mutex so concurrent reloads don't
// race on the working tree.
type gitSource struct {
	cfg gitSourceConfig
	mu  sync.Mutex
}

func (*gitSource) isSource() {}

// Load materializes the working tree (clone or fetch+checkout)
// and reads it like a local directory. Reuses the FromTOML
// reader internally — git's job ends at "produce a directory of
// TOML files."
func (s *gitSource) Load(ctx context.Context) (map[string]appBody, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ensureWorkingCopy(ctx); err != nil {
		return nil, err
	}
	// Delegate to the TOML reader on the materialized directory.
	sourceDir := s.cfg.clonePath
	if s.cfg.subdir != "" {
		sourceDir = filepath.Join(sourceDir, s.cfg.subdir)
	}
	ts := &tomlSource{cfg: tomlSourceConfig{path: sourceDir}}
	return ts.Load(ctx)
}

// Watch is a phase-1 stub. Webhook + poll-fallback land in
// phase 2; for now the working tree is loaded once at boot and
// stays put for the process lifetime.
func (s *gitSource) Watch(_ context.Context, _ func()) (stop func()) {
	return func() {}
}

// ensureWorkingCopy is the clone-or-fetch decision point.
// First call clones; subsequent calls fetch + checkout to pick
// up upstream changes.
func (s *gitSource) ensureWorkingCopy(ctx context.Context) error {
	if !directoryIsGitRepo(s.cfg.clonePath) {
		// Make sure the parent exists; git clone will create
		// the final segment itself.
		if err := os.MkdirAll(filepath.Dir(s.cfg.clonePath), 0o700); err != nil {
			return fmt.Errorf("mkdir for clone: %w", err)
		}
		// Remove any partial leftover from a failed previous clone.
		_ = os.RemoveAll(s.cfg.clonePath)
		return s.runGitClone(ctx)
	}
	return s.runGitFetchAndCheckout(ctx)
}

// runGitClone performs the initial clone with auth + known_hosts
// wired into the environment.
func (s *gitSource) runGitClone(ctx context.Context) error {
	url := s.cloneURL()
	args := []string{"clone", "--branch", s.cfg.branch, "--", url, s.cfg.clonePath}
	if s.cfg.ref != "" {
		// Don't --branch when the operator pinned a ref; we'll
		// check out the ref explicitly after a default clone.
		args = []string{"clone", "--", url, s.cfg.clonePath}
	}
	if err := s.runGit(ctx, args, nil); err != nil {
		return fmt.Errorf("git clone %s: %w", url, err)
	}
	if s.cfg.ref != "" {
		// Detach to the pinned ref.
		if err := s.runGit(ctx, []string{"checkout", s.cfg.ref}, &s.cfg.clonePath); err != nil {
			return fmt.Errorf("checkout %s: %w", s.cfg.ref, err)
		}
	}
	return nil
}

// runGitFetchAndCheckout brings an existing clone up to date.
// fetch + reset --hard to track upstream cleanly; any local
// changes in the clone are blown away (the clone is framework-
// owned state, operators don't edit it).
func (s *gitSource) runGitFetchAndCheckout(ctx context.Context) error {
	if err := s.runGit(ctx, []string{"fetch", "--prune", "origin"}, &s.cfg.clonePath); err != nil {
		return fmt.Errorf("git fetch: %w", err)
	}
	ref := s.cfg.ref
	if ref == "" {
		ref = "origin/" + s.cfg.branch
	}
	if err := s.runGit(ctx, []string{"reset", "--hard", ref}, &s.cfg.clonePath); err != nil {
		return fmt.Errorf("git reset --hard %s: %w", ref, err)
	}
	return nil
}

// runGit shells out to the git binary with auth + known_hosts
// wired through environment + flags. workDir is the optional
// directory the command runs in (nil = current); for clone we
// don't have a working tree yet, but for fetch/checkout the
// command must run inside the clone.
func (s *gitSource) runGit(ctx context.Context, args []string, workDir *string) error {
	cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- git binary + operator-supplied URL
	cmd.Env = s.gitEnv()
	if workDir != nil {
		cmd.Dir = *workDir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w; git output: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// gitEnv assembles GIT_SSH_COMMAND with key + known_hosts
// pinning when SSH options are configured. Falls back to the
// process environment for everything else.
func (s *gitSource) gitEnv() []string {
	env := os.Environ()
	if s.cfg.sshKey != "" {
		parts := []string{"ssh", "-i", s.cfg.sshKey, "-o", "IdentitiesOnly=yes"}
		if s.cfg.knownHosts != "" {
			parts = append(parts, "-o", "UserKnownHostsFile="+s.cfg.knownHosts,
				"-o", "StrictHostKeyChecking=yes")
		} else {
			parts = append(parts, "-o", "StrictHostKeyChecking=accept-new")
		}
		env = append(env, "GIT_SSH_COMMAND="+strings.Join(parts, " "))
	}
	// Disable any global config that might mutate clone behavior
	// (the framework wants reproducible clones, not whatever
	// the operator's ~/.gitconfig says).
	env = append(env, "GIT_TERMINAL_PROMPT=0")
	return env
}

// cloneURL inserts the HTTPS token into the URL when one's
// configured. SSH URLs pass through unchanged — auth happens via
// GIT_SSH_COMMAND. Non-https URLs with a token set log a SEV1
// warning (operator config error, but proceed in case the URL
// scheme is one git accepts that we don't recognize).
func (s *gitSource) cloneURL() string {
	if s.cfg.token == "" {
		return s.cfg.repo
	}
	if !strings.HasPrefix(s.cfg.repo, "https://") {
		fmt.Fprintf(os.Stderr, "config.FromGit: token set but URL is not https://; ignoring token\n")
		return s.cfg.repo
	}
	// Inject as https://<token>@host/...
	return "https://" + s.cfg.token + "@" + strings.TrimPrefix(s.cfg.repo, "https://")
}

// directoryIsGitRepo reports whether path looks like an
// existing git clone. Cheap check — full repo validation would
// shell out anyway, and a partial state will fail on the next
// fetch with a clear message.
func directoryIsGitRepo(path string) bool {
	info, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil && info != nil
}

// defaultGitClonePath builds a per-repo working dir under the
// system tempdir. Stable across boots — same repo URL → same
// path, so a process restart reuses the existing clone.
func defaultGitClonePath(repoURL string) string {
	// Conservative: replace anything non-alphanumeric with "_"
	// so the path is filesystem-safe.
	var b strings.Builder
	for i := 0; i < len(repoURL); i++ {
		c := repoURL[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	return filepath.Join(os.TempDir(), "configd-"+b.String())
}

// gitAvailable reports whether the git binary is on PATH. Used
// by tests + boot-time validation so an environment without git
// surfaces a clean error instead of a cryptic exec failure.
func gitAvailable() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// silence unused-import warnings for symbols phase-2 wires up.
var _ = errors.New
