// Package officialext registers extensions maintained and linked into Notch.
package officialext

import (
	"errors"
	"fmt"

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/officialext/askuser"
	"github.com/trobrock/notch/internal/officialext/explore"
	"github.com/trobrock/notch/internal/officialext/subagent"
	"github.com/trobrock/notch/internal/officialext/tasklist"
)

// Register registers all official extensions. Implementations remain isolated
// in subpackages so each extension can evolve and be tested independently.
func Register(registry *extension.Registry, host extension.Host) error {
	if registry == nil || host == nil {
		return errors.New("register official extensions: registry and host are required")
	}
	for _, register := range []func(*extension.Registry, extension.Host) error{
		askuser.Register,
		explore.Register,
		subagent.Register,
		tasklist.Register,
	} {
		if err := register(registry, host); err != nil {
			return fmt.Errorf("register official extensions: %w", err)
		}
	}
	return nil
}
