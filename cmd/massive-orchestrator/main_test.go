package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/spec"
)

func TestResolveStoreRootAppliesFlagOverEnvironment(t *testing.T) {
	t.Setenv("MASSIVE_STORE_PREFIX", "environment/prefix")
	base := t.TempDir()
	root, err := resolveStoreRoot(base, "flag/prefix", true)
	if err != nil {
		t.Fatal(err)
	}
	if root != filepath.Join(base, "flag", "prefix") {
		t.Fatalf("resolved store root = %q", root)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("prefix resolution created storage before run: %v", err)
	}
}

func TestRunRejectsInvalidStorePrefixBeforeReadingSpecOrWritingStore(t *testing.T) {
	for _, prefix := range []string{"../escape", "C:/absolute", "control\u0085key", "\ufefftenants/acme"} {
		t.Run(prefix, func(t *testing.T) {
			store := filepath.Join(t.TempDir(), "uncreated")
			_, err := run([]string{
				"run", "--spec", "missing.json", "--input", "null", "--store", store,
				"--store-prefix", prefix,
			})
			if err == nil || !strings.Contains(err.Error(), "invalid storage prefix") {
				t.Fatalf("run() error = %v, want invalid prefix", err)
			}
			if _, statErr := os.Stat(store); !os.IsNotExist(statErr) {
				t.Fatalf("invalid prefix created store: %v", statErr)
			}
		})
	}
}

func TestResolvePackageRootsKeepsLocationsOutOfWorkflowSpec(t *testing.T) {
	defaultRoot := t.TempDir()
	pythonRoot := t.TempDir()
	workflowSpec := &spec.WorkflowSpec{SourcePackages: map[string]spec.SourcePackage{
		"python-main":     {PackageID: "python-main"},
		"typescript-main": {PackageID: "typescript-main"},
	}}
	overrides := packageRootFlags{}
	if err := overrides.Set("python-main=" + pythonRoot); err != nil {
		t.Fatal(err)
	}

	resolved, err := resolvePackageRoots(workflowSpec, defaultRoot, overrides)
	if err != nil {
		t.Fatal(err)
	}
	if resolved["typescript-main"] != defaultRoot {
		t.Fatalf("default package root = %q, want %q", resolved["typescript-main"], defaultRoot)
	}
	wantPythonRoot, err := filepath.Abs(pythonRoot)
	if err != nil {
		t.Fatal(err)
	}
	if resolved["python-main"] != wantPythonRoot {
		t.Fatalf("overridden package root = %q, want %q", resolved["python-main"], wantPythonRoot)
	}
	if workflowSpec.SourcePackages["python-main"].PackageID != "python-main" {
		t.Fatal("operational binding mutated WorkflowSpec")
	}
}

func TestResolvePackageRootsRejectsUnknownAndDuplicateBindings(t *testing.T) {
	workflowSpec := &spec.WorkflowSpec{SourcePackages: map[string]spec.SourcePackage{
		"python-main": {PackageID: "python-main"},
	}}
	overrides := packageRootFlags{}
	if err := overrides.Set("unknown=/tmp/source"); err != nil {
		t.Fatal(err)
	}
	if _, err := resolvePackageRoots(workflowSpec, t.TempDir(), overrides); err == nil {
		t.Fatal("unknown source package root binding was accepted")
	}
	if err := overrides.Set("unknown=/tmp/second"); err == nil {
		t.Fatal("duplicate source package root binding was accepted")
	}
}
