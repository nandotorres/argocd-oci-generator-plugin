package auth

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/google/go-containerregistry/pkg/authn"
)

// ecrToken is a cached ECR authorization token.
type ecrToken struct {
	username  string
	password  string
	expiresAt time.Time
}

// fetchFunc obtains a fresh ECR token; injectable for tests.
type fetchFunc func(ctx context.Context, region, roleARN string) (ecrToken, error)

// ecrProvider caches ECR tokens keyed by region+role and refreshes them before
// expiry. ECR tokens are valid for 12h; we refresh with a safety margin.
type ecrProvider struct {
	mu    sync.Mutex
	cache map[string]ecrToken
	now   func() time.Time
	fetch fetchFunc
}

const ecrRefreshMargin = 15 * time.Minute

// logRefreshFailure reports a refresh failure that was absorbed by falling back
// to a still-valid cached token.
func (p *ecrProvider) logRefreshFailure(region, roleARN string, err error) {
	slog.Warn("ECR token refresh failed; using cached token until it expires",
		slog.String("region", region),
		slog.String("roleArn", roleARN),
		slog.Any("error", err))
}

func newECRProvider() *ecrProvider {
	return &ecrProvider{
		cache: map[string]ecrToken{},
		now:   time.Now,
		fetch: fetchECRToken,
	}
}

func (p *ecrProvider) authenticator(ctx context.Context, region, roleARN string) (authn.Authenticator, error) {
	key := region + "|" + roleARN

	p.mu.Lock()
	tok, ok := p.cache[key]
	p.mu.Unlock()

	if !ok || p.now().After(tok.expiresAt.Add(-ecrRefreshMargin)) {
		fresh, err := p.fetch(ctx, region, roleARN)
		switch {
		case err == nil:
			p.mu.Lock()
			p.cache[key] = fresh
			p.mu.Unlock()
			tok = fresh
		// A refresh can fail while the cached token is still valid: we refresh
		// ecrRefreshMargin early precisely so a transient STS/ECR failure does not
		// take us down. Keep using the cached token until it actually expires.
		case ok && p.now().Before(tok.expiresAt):
			p.logRefreshFailure(region, roleARN, err)
		default:
			return nil, fmt.Errorf("obtaining ECR token (region=%q role=%q): %w", region, roleARN, err)
		}
	}

	return authn.FromConfig(authn.AuthConfig{Username: tok.username, Password: tok.password}), nil
}

// fetchECRToken loads AWS config (IRSA / env / instance profile), optionally
// assumes roleARN, and calls ECR GetAuthorizationToken.
func fetchECRToken(ctx context.Context, region, roleARN string) (ecrToken, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return ecrToken{}, fmt.Errorf("loading AWS config: %w", err)
	}

	if roleARN != "" {
		stsClient := sts.NewFromConfig(cfg)
		cfg.Credentials = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(stsClient, roleARN))
	}

	out, err := ecr.NewFromConfig(cfg).GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return ecrToken{}, fmt.Errorf("GetAuthorizationToken: %w", err)
	}
	if len(out.AuthorizationData) == 0 || out.AuthorizationData[0].AuthorizationToken == nil {
		return ecrToken{}, fmt.Errorf("empty authorization data")
	}

	data := out.AuthorizationData[0]
	user, pass, err := decodeAuthToken(*data.AuthorizationToken)
	if err != nil {
		return ecrToken{}, err
	}

	expires := time.Now().Add(12 * time.Hour)
	if data.ExpiresAt != nil {
		expires = *data.ExpiresAt
	}
	return ecrToken{username: user, password: pass, expiresAt: expires}, nil
}

// decodeAuthToken decodes the base64 "user:password" ECR token.
func decodeAuthToken(b64 string) (string, string, error) {
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", "", fmt.Errorf("decoding ECR token: %w", err)
	}
	user, pass, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return "", "", fmt.Errorf("malformed ECR token")
	}
	return user, pass, nil
}
