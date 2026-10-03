// Package config loads and validates the plugin server configuration. Secrets
// are supplied via environment variables referenced as ${VAR}; they never travel
// over the ApplicationSet wire (see DESIGN.md §5).
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/nandotorres/argocd-oci-generator-plugin/internal/pattern"
)

// AuthType enumerates the supported credential providers.
type AuthType string

// Supported registry authentication types.
const (
	AuthAnonymous AuthType = "anonymous"
	AuthBasic     AuthType = "basic"
	AuthECR       AuthType = "ecr"
)

// Auth is the credential configuration for a registry.
type Auth struct {
	Type     AuthType `json:"type"`
	Username string   `json:"username,omitempty"`
	Password string   `json:"password,omitempty"`
	// ECR-specific.
	Region  string `json:"region,omitempty"`
	RoleARN string `json:"roleArn,omitempty"`
}

// Registry is a single configured registry and how to authenticate to it.
type Registry struct {
	Host                string   `json:"host"`
	Auth                Auth     `json:"auth"`
	AllowedRepositories []string `json:"allowedRepositories,omitempty"`

	allowed []*pattern.Pattern
}

// TLS controls transport security when talking to registries.
type TLS struct {
	// InsecureSkipVerify disables TLS certificate verification (use sparingly).
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
	// PlainHTTP talks to registries over http:// instead of https://. Intended
	// for local/dev registries only.
	PlainHTTP bool `json:"plainHTTP,omitempty"`
}

// Config is the top-level server configuration.
type Config struct {
	// Listen is the HTTP listen address, e.g. ":8080".
	Listen string `json:"listen,omitempty"`
	// Token is the bearer token the ApplicationSet controller must present.
	Token string `json:"token"`
	// DefaultRegistry is used when an ApplicationSet omits `registry`.
	DefaultRegistry string     `json:"defaultRegistry,omitempty"`
	Registries      []Registry `json:"registries,omitempty"`
	TLS             TLS        `json:"tls,omitempty"`
	// RequestTimeoutSeconds bounds a single getparams.execute call.
	RequestTimeoutSeconds int `json:"requestTimeoutSeconds,omitempty"`
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${VAR} with the environment value, erroring on unset vars
// so misconfiguration fails fast rather than silently authenticating as nobody.
func expandEnv(s string) (string, error) {
	var missing []string
	out := envRef.ReplaceAllStringFunc(s, func(m string) string {
		name := envRef.FindStringSubmatch(m)[1]
		v, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("unset environment variable(s): %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// Load reads, expands and validates the config file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	return Parse(raw)
}

// Parse expands and validates config from raw YAML/JSON bytes.
func Parse(raw []byte) (*Config, error) {
	var c Config
	if err := yaml.UnmarshalStrict(raw, &c); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if err := c.expand(); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	c.applyDefaults()
	return &c, nil
}

func (c *Config) expand() error {
	fields := []*string{&c.Token, &c.DefaultRegistry}
	for i := range c.Registries {
		r := &c.Registries[i]
		fields = append(fields, &r.Host, &r.Auth.Username, &r.Auth.Password, &r.Auth.Region, &r.Auth.RoleARN)
	}
	for _, f := range fields {
		v, err := expandEnv(*f)
		if err != nil {
			return err
		}
		*f = v
	}
	return nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.RequestTimeoutSeconds == 0 {
		c.RequestTimeoutSeconds = 60
	}
}

func (c *Config) validate() error {
	if strings.TrimSpace(c.Token) == "" {
		return fmt.Errorf("token is required (set it or reference ${PLUGIN_TOKEN})")
	}

	hosts := map[string]bool{}
	for i := range c.Registries {
		r := &c.Registries[i]
		if r.Host == "" {
			return fmt.Errorf("registries[%d]: host is required", i)
		}
		if hosts[r.Host] {
			return fmt.Errorf("registries[%d]: duplicate host %q", i, r.Host)
		}
		hosts[r.Host] = true

		switch r.Auth.Type {
		case AuthAnonymous, "":
			r.Auth.Type = AuthAnonymous
		case AuthBasic:
			if r.Auth.Username == "" || r.Auth.Password == "" {
				return fmt.Errorf("registries[%d] (%s): basic auth requires username and password", i, r.Host)
			}
		case AuthECR:
			// region/roleArn optional; credentials come from the AWS chain.
		default:
			return fmt.Errorf("registries[%d] (%s): invalid auth type %q", i, r.Host, r.Auth.Type)
		}

		for j, glob := range r.AllowedRepositories {
			p, err := pattern.Compile(glob)
			if err != nil {
				return fmt.Errorf("registries[%d].allowedRepositories[%d]: %w", i, j, err)
			}
			r.allowed = append(r.allowed, p)
		}
	}
	return nil
}

// RegistryFor returns the configured registry for a host, or nil if none.
func (c *Config) RegistryFor(host string) *Registry {
	for i := range c.Registries {
		if c.Registries[i].Host == host {
			return &c.Registries[i]
		}
	}
	return nil
}

// RepositoryAllowed reports whether the given repository is permitted for this
// registry. An empty allowlist permits everything.
func (r *Registry) RepositoryAllowed(repo string) bool {
	if len(r.allowed) == 0 {
		return true
	}
	for _, p := range r.allowed {
		if _, _, ok := p.Match(repo); ok {
			return true
		}
	}
	return false
}
