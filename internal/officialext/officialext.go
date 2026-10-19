// Package officialext registers extensions maintained and linked into Notch.
package officialext

import (
	"errors"
	"fmt"

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/officialext/askuser"
	"github.com/trobrock/notch/internal/officialext/explore"
	"github.com/trobrock/notch/internal/officialext/goal"
	"github.com/trobrock/notch/internal/officialext/modelswitch"
	"github.com/trobrock/notch/internal/officialext/monitor"
	"github.com/trobrock/notch/internal/officialext/plan"
	"github.com/trobrock/notch/internal/officialext/rtk"
	"github.com/trobrock/notch/internal/officialext/subagent"
	"github.com/trobrock/notch/internal/officialext/tasklist"
	"github.com/trobrock/notch/internal/officialext/team"
)

// Register registers all official extensions. Implementations remain isolated
// in subpackages so each extension can evolve and be tested independently.
func Register(registry *extension.Registry, host extension.Host) error {
	return RegisterWithSettingSources(registry, host, "user,project")
}

// RegisterWithSettingSources registers official extensions while propagating
// configuration isolation to child Notch processes.
func RegisterWithSettingSources(registry *extension.Registry, host extension.Host, settingSources string) error {
	return RegisterWithRuntime(registry, host, settingSources, "", "")
}

// RegisterWithRuntime also supplies the active parent model used by delegated
// helpers when they do not request an explicit model.
func RegisterWithRuntime(registry *extension.Registry, host extension.Host, settingSources, provider, modelName string) error {
	return RegisterWithRouting(registry, host, settingSources, provider, modelName, "", nil)
}

// RegisterWithRouting propagates an immutable profile snapshot to every child
// process used by all delegation tools.
func RegisterWithRouting(registry *extension.Registry, host extension.Host, settingSources, provider, modelName, routingSnapshot string, allowedProviders []string) error {
	if registry == nil || host == nil {
		return errors.New("register official extensions: registry and host are required")
	}
	exploreRunner, err := subagent.NewRunnerWithRouting(host, settingSources, provider, modelName, routingSnapshot, allowedProviders)
	if err != nil {
		return err
	}
	registrations := []func() error{
		func() error { return askuser.Register(registry, host) },
		func() error { return explore.RegisterWithRunner(registry, exploreRunner) },
		func() error { return goal.Register(registry, host) },
		func() error { return monitor.Register(registry, host) },
		func() error { return modelswitch.Register(registry, host) },
		func() error { return plan.Register(registry, host) },
		func() error { return rtk.Register(registry, host) },
		func() error {
			return subagent.RegisterWithRouting(registry, host, settingSources, provider, modelName, routingSnapshot, allowedProviders)
		},
		func() error {
			return team.RegisterWithRouting(registry, host, settingSources, provider, modelName, routingSnapshot, allowedProviders)
		},
		func() error { return tasklist.Register(registry, host) },
	}
	for _, register := range registrations {
		if err := register(); err != nil {
			return fmt.Errorf("register official extensions: %w", err)
		}
	}
	return nil
}
