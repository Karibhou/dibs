package store

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Baseline measured on main@1938d81 after #508. Do not raise it to accept new
// unreviewed sites; review those sites or fix their comparisons instead.
const casefoldBaseline = 0

// Advisory by default; CI observes in ratchet-warning mode before switching to
// ratchet enforcement. For local zero-hit enforcement, run
// DIBS_CASEFOLD_GATE=strict go test -count=1 -v ./pkg/store.
// This is a name-based tripwire, not data-flow analysis: aliases and identifier
// values hidden in other expressions need review. Index expressions with an
// identifier-shaped key are conservatively treated as map accesses.
//
// Allowlist entries that no longer point at a flagged site are a failure in
// every mode: a reason attached to a line that drifted exempts nothing, and
// the site it used to cover resurfaces as an unreviewed hit that an advisory
// run never surfaces in CI output.
func TestRepoIDCasefoldGate(t *testing.T) {
	hits, stale, err := casefoldAudit(filepath.Join("..", ".."), casefoldAllowlist, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) > 0 {
		t.Fatalf("%d stale casefold allowlist entries no longer mark a flagged site; re-pin them to the current line (and re-review) or delete them: %s", len(stale), strings.Join(stale, ", "))
	}
	mode := os.Getenv("DIBS_CASEFOLD_GATE")
	fails, err := casefoldGateFails(mode, hits, casefoldBaseline)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("casefold audit: %d unreviewed sites; baseline %d; mode %q", hits, casefoldBaseline, mode)
	if hits > casefoldBaseline {
		t.Logf("WARNING: casefold audit increased by %d sites above baseline; review diagnostics above", hits-casefoldBaseline)
	}
	if fails {
		t.Fatalf("%d unreviewed GitHub identifier comparison sites (baseline %d; see diagnostics above)", hits, casefoldBaseline)
	}
}

// ratchet-warning is CI's initial observation mode; see docs/casefold-gate.md
// for the switch to ratchet. Reject unknown modes so typos cannot disable it.
func casefoldGateFails(mode string, hits, baseline int) (bool, error) {
	switch mode {
	case "", "ratchet-warning":
		return false, nil
	case "ratchet":
		return hits > baseline, nil
	case "strict":
		return hits > 0, nil
	default:
		return false, fmt.Errorf("unknown DIBS_CASEFOLD_GATE mode %q (want empty, ratchet-warning, ratchet or strict)", mode)
	}
}

func TestCasefoldGatePolicy(t *testing.T) {
	for _, tc := range []struct {
		name, mode     string
		hits, baseline int
		want           bool
	}{
		{"advisory increase", "", 3, 2, false},
		{"warning increase", "ratchet-warning", 3, 2, false},
		{"warning baseline", "ratchet-warning", 2, 2, false},
		{"ratchet increase", "ratchet", 3, 2, true},
		{"ratchet baseline", "ratchet", 2, 2, false},
		{"ratchet decrease", "ratchet", 1, 2, false},
		{"ratchet zero baseline", "ratchet", 1, 0, true},
		{"strict existing hits", "strict", 2, 2, true},
		{"strict zero", "strict", 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := casefoldGateFails(tc.mode, tc.hits, tc.baseline)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("mode %q with %d hits and baseline %d: fail = %v, want %v", tc.mode, tc.hits, tc.baseline, got, tc.want)
			}
		})
	}
}

func TestCasefoldGateRejectsUnknownMode(t *testing.T) {
	for _, mode := range []string{"rachet", "ratchet_warning", "STRICT"} {
		t.Run(mode, func(t *testing.T) {
			// Even a clean audit must expose an invalid enforcement setting.
			_, err := casefoldGateFails(mode, 0, 0)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("unknown DIBS_CASEFOLD_GATE mode %q", mode)) {
				t.Fatalf("mode %q: got error %v, want unknown mode diagnostic", mode, err)
			}
		})
	}
}

// casefoldAudit scans non-test Go files under root/pkg and root/cmd. It
// returns the number of flagged sites absent from allowlist and the allowlist
// entries that matched no flagged site, sorted for stable diagnostics.
func casefoldAudit(root string, allowlist map[string]string, logf func(string, ...any)) (hits int, stale []string, err error) {
	fset := token.NewFileSet()
	used := map[string]bool{}
	for _, dir := range []string{"pkg", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			for _, pos := range casefoldSites(file) {
				p := fset.Position(pos)
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				site := fmt.Sprintf("%s:%d", filepath.ToSlash(rel), p.Line)
				if reason, ok := allowlist[site]; ok {
					used[site] = true
					logf("reviewed %s: %s", site, reason)
					continue
				}
				hits++
				logf("%s: use strings.EqualFold or canonicalise via the registry first", site)
			}
			return nil
		})
		if err != nil {
			return 0, nil, err
		}
	}
	for site := range allowlist {
		if !used[site] {
			stale = append(stale, site)
		}
	}
	sort.Strings(stale)
	return hits, stale, nil
}

// Keep exceptions narrow and explained. Line drift deliberately produces a new
// advisory hit for review, rather than exempting a whole file or expression;
// the drifted entry itself fails the gate so the hit cannot go unnoticed.
var casefoldAllowlist = map[string]string{
	"pkg/registry/registry.go:184": "description-enrichment memo is keyed by the hub profile spelling on both write and read; a respelled profile costs one extra lookup, never a wrong result",
	"pkg/registry/registry.go:214": "description-enrichment memo write uses the same hub profile spelling as the read above",
	"pkg/registry/registry.go:261": "reload preserves stored display spelling; lookupLocked supplies case-insensitive resolution",
	"pkg/registry/registry.go:308": "hub upsert resolves existing spelling through lookupLocked before storing",
	"pkg/registry/registry.go:381": "seed checks lookupLocked before inserting a new display spelling",
	"pkg/registry/registry.go:392": "exact lookup fast path falls back to strings.EqualFold",
	"pkg/history/history.go:101":   "reload restores the registry-canonical keys written by the backfiller",
	"pkg/history/history.go:123":   "API callers resolve through the registry before reading history",
	"pkg/history/history.go:136":   "persist-failure rollback snapshots the entry under the same registry List profile spelling it is about to overwrite",
	"pkg/history/history.go:137":   "backfiller writes registry List profile spelling",
	"pkg/history/history.go:140":   "persist-failure rollback restores the snapshot under the key written three lines above",
	"pkg/history/history.go:246":   "backfill work deduplication uses registry List profile spelling",
	"pkg/history/history.go:249":   "backfill work reservation uses the same registry-canonical key",
	"pkg/news/news.go:79":          "reload restores registry-canonical keys written by the generator",
	"pkg/news/news.go:92":          "API resolves the requested repo through the registry before reading news",
	"pkg/news/news.go:110":         "generator staleness check reads the same registry List profile key the generator writes",
	"pkg/news/news.go:118":         "generator cache lookup uses registry List profile spelling",
	"pkg/news/news.go:129":         "persist-failure rollback snapshots the entry under the same registry List profile spelling it is about to overwrite",
	"pkg/news/news.go:130":         "generator writes registry List profile spelling",
	"pkg/news/news.go:133":         "persist-failure rollback restores the snapshot under the key written three lines above",
	"pkg/news/news.go:348":         "generation work deduplication uses registry List profile spelling",
	"pkg/news/news.go:351":         "generation work reservation uses the same registry-canonical key",
	"pkg/match/engine.go:200":      "match cache entries are generated from registry profiles, not caller spelling",
	"pkg/match/engine.go:213":      "registry List supplies the same spelling used to build the match cache",
	"pkg/match/engine.go:251":      "cached matches and profile both carry registry spelling; hash also includes that spelling",
	"pkg/match/engine.go:262":      "replacement matches were generated from the same registry profile spelling",
	"pkg/match/engine.go:440":      "corpus lookup is built from registry List profiles",
	"pkg/match/engine.go:447":      "corpus results retain the spelling from those same registry profiles",
	"pkg/api/index.go:94":          "directory index is built from registry List profiles",
	"pkg/api/index.go:308":         "symbol lookup uses the same registry List spelling used to generate symbols",
	"pkg/api/index.go:347":         "repoID has been replaced by Registry.Get's canonical profile spelling",
}

func casefoldSites(file *ast.File) []token.Pos {
	var sites []token.Pos
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if casefoldCall(n, "strings", "EqualFold") {
				return false
			}
		case *ast.BinaryExpr:
			if (n.Op == token.EQL || n.Op == token.NEQ) &&
				!casefoldEmpty(n.X) && !casefoldEmpty(n.Y) &&
				(casefoldIdentifier(n.X) || casefoldIdentifier(n.Y)) {
				sites = append(sites, n.Pos())
			}
		case *ast.SwitchStmt:
			if casefoldIdentifier(n.Tag) {
				sites = append(sites, n.Tag.Pos())
			}
		case *ast.IndexExpr:
			if casefoldIdentifier(n.Index) {
				sites = append(sites, n.Pos())
			}
		}
		return true
	})
	return sites
}

func casefoldIdentifier(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.ParenExpr:
		return casefoldIdentifier(e.X)
	case *ast.Ident:
		return casefoldName(e.Name)
	case *ast.SelectorExpr:
		return casefoldName(e.Sel.Name)
	case *ast.IndexExpr:
		// PassedRepos is a slice of GitHub identifiers, not an identifier
		// itself. Recognise its elements in comparisons and map keys.
		switch x := e.X.(type) {
		case *ast.Ident:
			return x.Name == "PassedRepos"
		case *ast.SelectorExpr:
			return x.Sel.Name == "PassedRepos"
		}
	}
	// Calls, including strings.ToLower and store.CanonicalRepoID, are
	// outside this raw-operand check; aliases/wrappers need human review.
	return false
}

func casefoldName(name string) bool {
	switch name {
	case "RepoID", "TargetRepo", "Owner", "User", "Login", "Author", "Username", "repoID", "owner", "user", "login", "author", "viewer":
		return true
	}
	return false
}

func casefoldEmpty(e ast.Expr) bool {
	if p, ok := e.(*ast.ParenExpr); ok {
		return casefoldEmpty(p.X)
	}
	lit, ok := e.(*ast.BasicLit)
	// Empty checks do not compare GitHub identities.
	return ok && lit.Kind == token.STRING && (lit.Value == `""` || lit.Value == "``")
}

func casefoldCall(call *ast.CallExpr, pkg, name string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

func TestCasefoldSites(t *testing.T) {
	for _, tc := range []struct {
		name, stmt string
		want       int
	}{
		{"repo equality", `if o.RepoID == rp.RepoID {}`, 1},
		{"owner inequality", `if owner != rp.Owner {}`, 1},
		{"user", `if user == x.User {}`, 1},
		{"login", `if x.Login != login {}`, 1},
		{"target", `if i.TargetRepo == repoID {}`, 1},
		{"passed element", `if i.PassedRepos[k] == candidate {}`, 1},
		{"passed key", `m[i.PassedRepos[k]] = true`, 1},
		{"switch", `switch (rp.Owner) { case "alice": }`, 1},
		{"map read", `_ = m[o.RepoID]`, 1},
		{"map write", `m[repoID] = true`, 1},
		{"folded map", `m[strings.ToLower(o.RepoID)] = true`, 0},
		{"canonical map", `_ = m[store.CanonicalRepoID(repoID)]`, 0},
		{"folded equality", `if strings.ToLower(owner) == strings.ToLower(login) {}`, 0},
		{"half folded", `if strings.ToLower(owner) == login {}`, 1},
		{"equal fold", `if strings.EqualFold(o.RepoID, repoID) {}`, 0},
		{"empty", `if (i.TargetRepo) != ("") {}`, 0},
		{"empty reversed", "if `` == user {}", 0},
		{"unrelated", `if status == "accepted" {}`, 0},
		{"slice index", `_ = i.PassedRepos[k]`, 0},
		{"folded switch", `switch strings.ToLower(login) { case "alice": }`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := parser.ParseFile(token.NewFileSet(), "fixture.go", "package fixture; func f() {"+tc.stmt+"}", 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(casefoldSites(f)); got != tc.want {
				t.Fatalf("got %d hits, want %d for %s", got, tc.want, tc.stmt)
			}
		})
	}
}

func TestCasefoldAuditReportsStaleAllowlistEntries(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pkg", "fixture")
	for _, d := range []string{dir, filepath.Join(root, "cmd")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	src := "package fixture\n\nfunc f(m map[string]bool, repoID, other string) bool {\n\tm[repoID] = true // line 4: flagged\n\treturn other == \"x\"\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture_test.go"), []byte("package fixture\n\nfunc g(repoID string, m map[string]bool) { m[repoID] = true }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	hits, stale, err := casefoldAudit(root, map[string]string{
		"pkg/fixture/fixture.go:4": "fixture key is canonical",
		"pkg/fixture/fixture.go:5": "drifted: line 5 is not a flagged site",
		"pkg/fixture/gone.go:1":    "drifted: file no longer exists",
	}, logf)
	if err != nil {
		t.Fatal(err)
	}
	if hits != 0 {
		t.Fatalf("hits = %d, want 0 (the only site is allowlisted); logs: %q", hits, logs)
	}
	want := []string{"pkg/fixture/fixture.go:5", "pkg/fixture/gone.go:1"}
	if strings.Join(stale, ",") != strings.Join(want, ",") {
		t.Fatalf("stale = %q, want %q", stale, want)
	}
	if len(logs) != 1 || !strings.HasPrefix(logs[0], "reviewed pkg/fixture/fixture.go:4:") {
		t.Fatalf("logs = %q, want exactly one reviewed line for the allowlisted site (test files must be skipped)", logs)
	}

	hits, stale, err = casefoldAudit(root, nil, logf)
	if err != nil {
		t.Fatal(err)
	}
	if hits != 1 || len(stale) != 0 {
		t.Fatalf("without allowlist: hits = %d, stale = %q; want 1 hit and no stale entries", hits, stale)
	}
}
