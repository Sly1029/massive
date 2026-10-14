//go:build windows

package orchestrator

import "fmt"

func claimCoordinator(root, project, run string) (func(), error) {
	return nil, fmt.Errorf("coordinator capability: this local spike requires Unix ownership locks")
}
