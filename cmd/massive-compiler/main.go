package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Sly1029/massive/internal/deployment"
	"github.com/Sly1029/massive/internal/orchestrator"
	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/spec"
	"github.com/Sly1029/massive/internal/target/argo"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		var diagnostics *spec.DiagnosticsError
		if errors.As(err, &diagnostics) {
			for _, diagnostic := range diagnostics.Diagnostics {
				fmt.Fprintf(os.Stderr, "invalid workflow spec: %s\n", diagnostic.String())
			}
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "massive-compiler: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("expected subcommand: compile or bundle-argo")
	}
	if args[0] == "bundle-argo" {
		return bundleArgo(args[1:])
	}
	if args[0] != "compile" {
		return fmt.Errorf("unknown subcommand %q", args[0])
	}

	flags := flag.NewFlagSet("compile", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	specPath := flags.String("spec", "", "workflow spec JSON file")
	outDir := flags.String("out", "", "output directory")
	if err := flags.Parse(args[1:]); err != nil {
		return fmt.Errorf("parse compile flags: %w", err)
	}
	if *specPath == "" {
		return fmt.Errorf("compile requires --spec")
	}
	if *outDir == "" {
		return fmt.Errorf("compile requires --out")
	}

	specData, err := os.ReadFile(*specPath)
	if err != nil {
		return fmt.Errorf("read spec %q: %w", *specPath, err)
	}
	workflowSpec, err := spec.Parse(specData)
	if err != nil {
		return err
	}
	result, err := plan.Compile(workflowSpec, specData)
	if err != nil {
		return fmt.Errorf("compile workflow plan: %w", err)
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return fmt.Errorf("create output directory %q: %w", *outDir, err)
	}
	outputPath := filepath.Join(*outDir, "workflow-plan.json")
	if err := os.WriteFile(outputPath, result.CanonicalJSON, 0o644); err != nil {
		return fmt.Errorf("write workflow plan %q: %w", outputPath, err)
	}

	fmt.Printf("compiled workflow %q: %s -> %s\n", workflowSpec.Workflow.Name, result.PlanHash, outputPath)
	return nil
}

func bundleArgo(args []string) error {
	flags := flag.NewFlagSet("bundle-argo", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	planPath := flags.String("plan", "", "canonical workflow plan JSON")
	deploymentPath := flags.String("deployment", "", "DeploymentSpec JSON")
	outDir := flags.String("out", "", "bundle output directory")
	runtimeAssets := flags.String("runtime-assets", "", "directory containing source-sha256-<hash>.tar files")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse bundle-argo flags: %w", err)
	}
	if *planPath == "" || *deploymentPath == "" || *outDir == "" || *runtimeAssets == "" {
		return fmt.Errorf("bundle-argo requires --plan, --deployment, --runtime-assets, and --out")
	}
	planJSON, err := os.ReadFile(*planPath)
	if err != nil {
		return fmt.Errorf("read plan %q: %w", *planPath, err)
	}
	d, err := deployment.ReadFile(*deploymentPath)
	if err != nil {
		return err
	}
	parsedPlan, err := plan.ParseCanonicalJSON(planJSON)
	if err != nil {
		return err
	}
	archives := make(map[string][]byte, len(parsedPlan.GetSourcePackages()))
	for _, sourcePackage := range parsedPlan.GetSourcePackages() {
		name, err := orchestrator.SourceArchiveBundleName(sourcePackage.GetPackageHash())
		if err != nil {
			return err
		}
		body, err := os.ReadFile(filepath.Join(*runtimeAssets, name))
		if err != nil {
			return fmt.Errorf("read runtime source archive %q: %w", name, err)
		}
		archives[sourcePackage.GetPackageHash()] = body
	}
	b, err := argo.Compile(planJSON, d, argo.RuntimeAssets{SourceArchives: archives})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	for _, f := range b.Files {
		path := filepath.Join(*outDir, f.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("create bundle path %s: %w", f.Path, err)
		}
		if err := os.WriteFile(path, f.Bytes, 0o644); err != nil {
			return fmt.Errorf("write bundle file %s: %w", f.Path, err)
		}
	}
	if err := os.WriteFile(filepath.Join(*outDir, "bundle-manifest.json"), b.ManifestJSON, 0o644); err != nil {
		return err
	}
	fmt.Printf("bundled argo plan %s -> %s\n", d.PlanHash, *outDir)
	return nil
}
