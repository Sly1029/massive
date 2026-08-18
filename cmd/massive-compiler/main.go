package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Sly1029/massive/internal/deployment"
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
	if err := os.WriteFile(outputPath, append(result.CanonicalJSON, '\n'), 0o644); err != nil {
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
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse bundle-argo flags: %w", err)
	}
	if *planPath == "" || *deploymentPath == "" || *outDir == "" {
		return fmt.Errorf("bundle-argo requires --plan, --deployment, and --out")
	}
	planJSON, err := os.ReadFile(*planPath)
	if err != nil {
		return fmt.Errorf("read plan %q: %w", *planPath, err)
	}
	d, err := deployment.ReadFile(*deploymentPath)
	if err != nil {
		return err
	}
	b, err := argo.Compile(planJSON, d)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	for _, f := range b.Files {
		if err := os.WriteFile(filepath.Join(*outDir, f.Path), f.Bytes, 0o644); err != nil {
			return fmt.Errorf("write bundle file %s: %w", f.Path, err)
		}
	}
	if err := os.WriteFile(filepath.Join(*outDir, "bundle-manifest.json"), append(b.ManifestJSON, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("bundled argo plan %s -> %s\n", d.PlanHash, *outDir)
	return nil
}
