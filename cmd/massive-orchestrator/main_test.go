package main

import (
	"path/filepath"
	"testing"

	"github.com/Sly1029/massive/internal/spec"
)

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
