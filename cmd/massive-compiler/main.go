package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/spec"
	"github.com/Sly1029/massive/internal/target"
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
		return fmt.Errorf("expected subcommand: compile | compile-target")
	}
	switch args[0] {
	case "compile":
		return runCompile(args[1:])
	case "compile-target":
		return runCompileTarget(args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q (expected compile | compile-target)", args[0])
	}
}

func runCompile(args []string) error {
	flags := flag.NewFlagSet("compile", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	specPath := flags.String("spec", "", "workflow spec JSON file")
	outDir := flags.String("out", "", "output directory")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse compile flags: %w", err)
	}
	if *specPath == "" {
		return fmt.Errorf("compile requires --spec")
	}
	if *outDir == "" {
		return fmt.Errorf("compile requires --out")
	}

	_, result, err := compilePlan(*specPath)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return fmt.Errorf("create output directory %q: %w", *outDir, err)
	}
	outputPath := filepath.Join(*outDir, "workflow-plan.json")
	if err := os.WriteFile(outputPath, append(result.CanonicalJSON, '\n'), 0o644); err != nil {
		return fmt.Errorf("write workflow plan %q: %w", outputPath, err)
	}

	fmt.Printf("compiled workflow: %s -> %s\n", result.PlanHash, outputPath)
	return nil
}

// runCompileTarget compiles a spec to a plan and then lowers a requested target
// to a deploy bundle. The bundle directory defaults to dist/<target>/<workflow>/
// per docs/spec/argo-backend.md.
func runCompileTarget(args []string) error {
	flags := flag.NewFlagSet("compile-target", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	specPath := flags.String("spec", "", "workflow spec JSON file")
	targetKind := flags.String("target", "", "target backend id (for example: argo)")
	bundleOut := flags.String("bundle-out", "", "bundle output directory (default dist/<target>/<workflow>/)")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse compile-target flags: %w", err)
	}
	if *specPath == "" {
		return fmt.Errorf("compile-target requires --spec")
	}
	if *targetKind == "" {
		return fmt.Errorf("compile-target requires --target")
	}

	workflowSpec, result, err := compilePlan(*specPath)
	if err != nil {
		return err
	}

	targetRequest, err := resolveTargetRequest(workflowSpec, *targetKind)
	if err != nil {
		return err
	}

	registry := target.NewRegistry()
	registry.Register(argo.New())

	bundle, err := registry.Compile(*targetKind, target.CompileInput{
		Plan:         result.Plan,
		PlanJSON:     result.CanonicalJSON,
		PlanHash:     result.PlanHash,
		TargetKind:   targetRequest.Kind,
		TargetConfig: targetRequest.Config,
	})
	if err != nil {
		return err
	}

	bundleDir := *bundleOut
	if bundleDir == "" {
		bundleDir = filepath.Join("dist", *targetKind, workflowSpec.Workflow.Name)
	}
	if err := target.WriteBundle(bundleDir, bundle); err != nil {
		return err
	}

	fmt.Printf("compiled %s bundle: plan %s bundle %s -> %s\n", *targetKind, bundle.Manifest.GetPlanHash(), bundle.Manifest.GetBundleHash(), bundleDir)
	for _, file := range bundle.Manifest.GetFiles() {
		fmt.Printf("  %s\n", file.GetPath())
	}
	fmt.Printf("  %s\n", target.BundleManifestPath)
	return nil
}

func compilePlan(specPath string) (*spec.WorkflowSpec, *plan.CompileResult, error) {
	specData, err := os.ReadFile(specPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read spec %q: %w", specPath, err)
	}
	workflowSpec, err := spec.Parse(specData)
	if err != nil {
		return nil, nil, err
	}
	result, err := plan.Compile(workflowSpec, specData)
	if err != nil {
		return nil, nil, fmt.Errorf("compile workflow plan: %w", err)
	}
	return workflowSpec, result, nil
}

func resolveTargetRequest(workflowSpec *spec.WorkflowSpec, kind string) (spec.Target, error) {
	declared := make([]string, 0, len(workflowSpec.Targets))
	for _, request := range workflowSpec.Targets {
		if request.Kind == kind {
			return request, nil
		}
		declared = append(declared, request.Kind)
	}
	if len(declared) == 0 {
		return spec.Target{}, fmt.Errorf("workflow %q declares no targets; cannot compile target %q", workflowSpec.Workflow.Name, kind)
	}
	return spec.Target{}, fmt.Errorf("workflow %q does not request target %q; declared targets: %v", workflowSpec.Workflow.Name, kind, declared)
}
