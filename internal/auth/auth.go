// Package auth resolves per-registry credentials into go-containerregistry
// authenticators. Credentials are sourced exclusively from server-side
// configuration and cloud identity (never from the ApplicationSet), per the
// security model in DESIGN.md §5.
package auth

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/nandotorres/argocd-oci-generator-plugin/internal/config"
)

// Provider turns a registry host into an authenticator for a given request.
type Provider interface {
	Authenticator(ctx context.Context, host string) (authn.Authenticator, error)
}

// Resolver implements Provider using the server configuration. ECR tokens are
// cached until shortly before expiry.
type Resolver struct {
	cfg *config.Config
	ecr *ecrProvider
}

// NewResolver builds a Resolver from config.
func NewResolver(cfg *config.Config) *Resolver {
	return &Resolver{cfg: cfg, ecr: newECRProvider()}
}

// Authenticator returns the authenticator for the given host.
func (r *Resolver) Authenticator(ctx context.Context, host string) (authn.Authenticator, error) {
	reg := r.cfg.RegistryFor(host)
	if reg == nil {
		// Unknown registries are treated as anonymous. Access control is enforced
		// separately via the repository allowlist.
		return authn.Anonymous, nil
	}

	switch reg.Auth.Type {
	case config.AuthAnonymous, "":
		return authn.Anonymous, nil
	case config.AuthBasic:
		return authn.FromConfig(authn.AuthConfig{
			Username: reg.Auth.Username,
			Password: reg.Auth.Password,
		}), nil
	case config.AuthECR:
		return r.ecr.authenticator(ctx, reg.Auth.Region, reg.Auth.RoleARN)
	default:
		return nil, fmt.Errorf("unsupported auth type %q for host %s", reg.Auth.Type, host)
	}
}
