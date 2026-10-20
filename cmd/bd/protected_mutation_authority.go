package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/rhar1511/beads/internal/httpapi"
	"github.com/rhar1511/beads/internal/protectedmutation"
	"github.com/rhar1511/beads/internal/storage/domain"
	"github.com/rhar1511/beads/internal/storage/schema"
)

// This variable is a supervisor-owned startup input. Its absolute path may
// name only a protected host mount; bd never reads a workspace setting or
// request member to select trusted permit keys.
const serveProtectedMutationAuthorityEnv = "BEADS_SERVE_PROTECTED_MUTATION_AUTHORITY_FILE"

type protectedMutationReadinessSource interface {
	ProtectedMutationReadiness(context.Context) (schema.ProtectedMutationReadiness, error)
}

// resolveServeProtectedMutationAuthority reads one supervisor-selected
// host authority file and binds its separate audience, project, and database
// fields to the ContextInfo snapshot selected for this server. The zero value
// is returned only when the setting is absent. Once present, every missing or
// invalid authority input is a startup error.
func resolveServeProtectedMutationAuthority(
	ctx context.Context,
	workspace domain.ContextInfo,
	readinessSource any,
) (httpapi.ProtectedMutationAuthority, func() error, error) {
	path, configured := os.LookupEnv(serveProtectedMutationAuthorityEnv)
	if !configured {
		return httpapi.ProtectedMutationAuthority{}, func() error { return nil }, nil
	}
	if path == "" {
		return httpapi.ProtectedMutationAuthority{}, nil, fmt.Errorf("%s is set but empty", serveProtectedMutationAuthorityEnv)
	}
	source, binding, err := protectedmutation.NewFileHostAuthoritySource(path)
	if err != nil {
		return httpapi.ProtectedMutationAuthority{}, nil, fmt.Errorf("%s: %w", serveProtectedMutationAuthorityEnv, err)
	}
	closeSource := func(err error) (httpapi.ProtectedMutationAuthority, func() error, error) {
		if closeErr := source.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close protected mutation authority: %w", closeErr))
		}
		return httpapi.ProtectedMutationAuthority{}, nil, err
	}
	if workspace.ProjectID == "" || workspace.Database == "" || binding.ProjectID != workspace.ProjectID || binding.Database != workspace.Database {
		return closeSource(fmt.Errorf("protected mutation authority project/database binding does not match the selected workspace"))
	}
	verifier, err := protectedmutation.NewVerifier(source, protectedmutation.Options{
		Audience: binding.Audience, ProjectID: binding.ProjectID, Database: binding.Database,
	})
	if err != nil {
		return closeSource(fmt.Errorf("construct protected mutation verifier: %w", err))
	}
	if err := verifier.ValidateStartup(ctx); err != nil {
		return closeSource(fmt.Errorf("validate protected mutation authority before serving: %w", err))
	}
	readiness, ok := readinessSource.(protectedMutationReadinessSource)
	if !ok {
		return closeSource(fmt.Errorf("selected storage source does not expose protected mutation schema readiness"))
	}
	proof, err := readiness.ProtectedMutationReadiness(ctx)
	if err != nil {
		return closeSource(fmt.Errorf("probe protected mutation schema readiness: %w", err))
	}
	if !proof.Ready() {
		return closeSource(fmt.Errorf("selected database is not ready for protected mutation enforcement"))
	}
	authority, err := httpapi.NewProtectedMutationAuthority(
		verifier, binding.Audience, binding.ProjectID, binding.Database, proof,
	)
	if err != nil {
		return closeSource(fmt.Errorf("construct protected mutation authority: %w", err))
	}
	return authority, verifier.Close, nil
}
