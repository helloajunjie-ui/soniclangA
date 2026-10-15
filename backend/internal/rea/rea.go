// Package rea implements the REA auxiliary toolset (DESIGN §M9): a
// Retrieve/Extend/Act (or Reasoning/Execution/Assessment) loop that augments
// the main orchestrator with external grounding and self-critique. M2 ships the
// service, the enable flag and safe no-op seams; the retrieval grounding,
// action extension and self-assessment workers are wired in a later milestone.
package rea

import (
	"context"
	"errors"
	"fmt"
)

// errDisabled is returned when REA is not enabled.
var errDisabled = errors.New("REA auxiliary tools are disabled")

// Service is the REA backend.
type Service struct {
	Enabled bool
}

// New constructs the REA Service.
func New(enabled bool) *Service {
	return &Service{Enabled: enabled}
}

// IsEnabled reports whether REA is active.
func (s *Service) IsEnabled() bool { return s.Enabled }

// Suggest returns an auxiliary reasoning suggestion for a prompt. M2 returns a
// placeholder when disabled, and a not-implemented note when enabled.
func (s *Service) Suggest(_ string) (string, error) {
	if !s.Enabled {
		return "", errDisabled
	}
	return "", errors.New("REA suggest is not implemented in M2")
}

// Act performs an auxiliary action (extend/assess). M2 is a no-op seam.
func (s *Service) Act(_ context.Context, action string, _ map[string]interface{}) (string, error) {
	if !s.Enabled {
		return "", errDisabled
	}
	return "", fmt.Errorf("REA action %s is not implemented in M2", action)
}
