package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Sly1029/massive/internal/plan"
	"github.com/Sly1029/massive/internal/spec"
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
		return fmt.Errorf("expected subcommand: compile")
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
