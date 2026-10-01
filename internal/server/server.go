// Package server implements the ApplicationSet plugin generator HTTP contract:
// POST /api/v1/getparams.execute, authenticated with a bearer token.
//
// Critically, any failure to produce a complete, correct result is returned as a
// non-2xx response. The ApplicationSet controller treats that as a generator
// error and leaves existing Applications untouched (DESIGN.md §2.1, §7).
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/torres/argocd-oci-generator-plugin/internal/config"
	"github.com/torres/argocd-oci-generator-plugin/internal/generator"
)

// Generator is the behaviour the server needs; satisfied by *generator.Generator.
type Generator interface {
	Generate(ctx context.Context, q *generator.Query) ([]map[string]any, error)
}

// Server serves the plugin API.
type Server struct {
	cfg *config.Config
	gen Generator
	log *slog.Logger
}

// New creates a Server.
func New(cfg *config.Config, gen Generator, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{cfg: cfg, gen: gen, log: log}
}

// Handler returns the HTTP handler with all routes wired.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/getparams.execute", s.requireToken(s.handleGetParams))
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleHealth)
	return mux
}

// --- request/response types (mirror applicationset/services/plugin) ---

type serviceRequest struct {
	ApplicationSetName string `json:"applicationSetName"`
	Input              struct {
		Parameters json.RawMessage `json:"parameters"`
	} `json:"input"`
}

type output struct {
	Parameters []map[string]any `json:"parameters"`
}

type serviceResponse struct {
	Output output `json:"output"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// requireToken enforces the bearer token using a constant-time comparison.
func (s *Server) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		if auth == "" || token == auth || subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.Token)) != 1 {
			s.writeError(w, r, http.StatusUnauthorized, errors.New("invalid or missing bearer token"))
			return
		}
		next(w, r)
	}
}

func (s *Server) handleGetParams(w http.ResponseWriter, r *http.Request) {
	timeout := time.Duration(s.cfg.RequestTimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	var req serviceRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, errors.New("invalid request body: "+err.Error()))
		return
	}

	var in generator.Input
	if len(req.Input.Parameters) > 0 {
		pdec := json.NewDecoder(strings.NewReader(string(req.Input.Parameters)))
		pdec.DisallowUnknownFields()
		if err := pdec.Decode(&in); err != nil {
			s.writeError(w, r, http.StatusBadRequest, errors.New("invalid input.parameters: "+err.Error()))
			return
		}
	}

	log := s.log.With(
		slog.String("applicationSet", req.ApplicationSetName),
		slog.String("registry", in.Registry),
		slog.String("repository", in.Repository),
	)

	q, err := in.Compile(s.cfg.DefaultRegistry)
	if err != nil {
		log.Warn("invalid generator input", slog.Any("error", err))
		s.writeError(w, r, http.StatusBadRequest, err)
		return
	}

	reg := s.cfg.RegistryFor(q.Registry)
	if reg == nil {
		err := errors.New("registry " + q.Registry + " is not configured on the plugin server")
		log.Warn("registry not configured")
		s.writeError(w, r, http.StatusForbidden, err)
		return
	}
	q.AllowRepository = reg.RepositoryAllowed

	params, err := s.gen.Generate(ctx, q)
	if err != nil {
		// Fail closed: upstream/registry/policy errors must not yield a 2xx.
		log.Error("generation failed", slog.Any("error", err))
		s.writeError(w, r, http.StatusBadGateway, err)
		return
	}

	log.Info("generated parameters", slog.Int("count", len(params)))
	s.writeJSON(w, http.StatusOK, serviceResponse{Output: output{Parameters: params}})
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Error("failed to encode response", slog.Any("error", err))
	}
}

func (s *Server) writeError(w http.ResponseWriter, _ *http.Request, status int, err error) {
	s.writeJSON(w, status, errorResponse{Error: err.Error()})
}
