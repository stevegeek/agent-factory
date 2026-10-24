// Package archtest enforces the architecture plan's structural rules
// (docs/go-rewrite/ARCHITECTURE.md §2.2, §8.5, §10.1) as tests, so a broken
// boundary is a build failure rather than a review point.
//
// The checks run against any module directory, so the package's own tests
// can build small fixture modules that break each rule. The repository test
// (TestRepository) runs every check against the real module.
//
// Rules:
//
//  1. The allowed-imports table (Table). Every internal import edge, in
//     production and test code, must be listed. A package that is not in the
//     table fails. Tests may also import internal/testkit. Unbuilt packages
//     are simply absent.
//  2. internal/testkit is test-only: production code never imports it.
//  3. The Foreman closure: the dependencies of cmd/foreman never include a
//     credentialed or writing package (ForemanForbidden).
//  4. The helper closure: the dependencies of cmd/factory-remote stay inside
//     HelperAllowed; those of cmd/factory-pull inside PullAllowed.
//  5. Standard library only: no dependency outside std and the module, and no
//     require directive in go.mod.
//  6. Package size: no internal package exceeds MaxPackageLines lines of
//     non-test Go.
//  7. No package-level mutable state: a package-level var is allowed only if
//     its type is *regexp.Regexp, embed.FS, fs.FS initialised from an
//     embed.FS, or error from errors.New (plus the two linker-set strings of
//     internal/buildinfo), and nothing assigns to it after its declaration.
//  8. Port markers: every "// port: TF Class.test_name" marker names a Python
//     test in testdata/python-tests.txt (frozen from tests/*.py at the
//     cutover, so the check needs no Python), and every listed test must have
//     a marker (FACTORY_PORT_COMPLETE=0 relaxes that).
//  9. Regex goldens: every regexp.Compile/MustCompile in production code
//     carries either "// py: <python pattern>", whose pattern appears in a
//     regex golden file (internal/*/testdata/regex*.json, written from the
//     Python before the cutover and static since), or "// re: go" for a
//     pattern that is not a Python port (for example one copied from
//     Foreman).
//  10. Hermetic tests: no test outside internal/testkit reads HOME or
//     FACTORY_ROOT from the environment (os.Getenv, os.LookupEnv,
//     os.UserHomeDir).
//  11. No personal strings: when FACTORY_PERSONAL_STRINGS names a file (kept
//     in the private config repo), no file in the module contains one of its
//     lines.
//  12. Domain purity (§2.2 rule 2): internal/model and internal/ticket
//     import no os, os/..., syscall, net or net/... package and never refer
//     to time.Now.
//  13. Interface size (§2.2 rule 6): no exported interface in production
//     code has more than four methods, counting embedded ones.
//  14. Reasons (§3.2, §3.5): no model.Reason is spelled as a string literal
//     outside internal/model, and every model.Reason constant is written
//     (not only compared) by production code outside internal/model.
package archtest
