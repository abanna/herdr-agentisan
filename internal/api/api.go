// Package api serves the REST API over the notes domain.
//
// Routes are registered in one place (Router) so the OpenAPI document can be
// generated from the same table the server binds — see spec.go. Handlers map
// domain errors onto status codes; they never inspect error strings.
package api

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/trace"

	"github.com/nerds-run/go-agents/internal/config"
	"github.com/nerds-run/go-agents/internal/logging"
	"github.com/nerds-run/go-agents/internal/notes"
)

// Server holds the dependencies every handler needs.
type Server struct {
	store notes.Store
	log   zerolog.Logger
	// token, when non-empty, is required as a bearer token on mutating routes.
	token string
	// observers run before the request logger. Telemetry is injected this way
	// so this package never imports internal/telemetry: the router does not
	// need to know what is watching it, and tests can build one without an
	// exporter.
	observers []gin.HandlerFunc
}

// Option customises a Server. Options are variadic so adding one never breaks
// an existing NewServer call.
type Option func(*Server)

// WithObservers installs middleware between gin.Recovery and the request
// logger — the position where a tracing middleware must sit for the logger to
// find its span, and where a metrics middleware still times the whole chain.
func WithObservers(mw ...gin.HandlerFunc) Option {
	return func(s *Server) { s.observers = append(s.observers, mw...) }
}

// NewServer builds a Server. An empty token disables auth, which Config only
// permits in development.
func NewServer(store notes.Store, log zerolog.Logger, token string, opts ...Option) *Server {
	s := &Server{store: store, log: log, token: token}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ErrorBody is the single error shape every failing route returns.
type ErrorBody struct {
	Error string `json:"error"`
}

// Router builds the gin engine with every route and middleware attached.
func (s *Server) Router() *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(s.observers...)
	r.Use(s.requestLogger())

	r.GET("/healthz", s.health)
	r.GET("/readyz", s.ready)
	r.GET("/version", s.version)

	v1 := r.Group("/v1")
	{
		v1.GET("/notes", s.listNotes)
		v1.GET("/notes/:id", s.getNote)

		guarded := v1.Group("", s.requireToken())
		guarded.POST("/notes", s.createNote)
		guarded.DELETE("/notes/:id", s.deleteNote)
	}
	return r
}

// requestLogger logs one line per request and puts the logger in the context
// so handlers and the domain can log with the same request-scoped fields.
func (s *Server) requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		builder := s.log.With().
			Str("method", c.Request.Method).
			Str("path", c.Request.URL.Path)
		// A log line and a span are only useful together if one names the
		// other. When a tracing observer is installed the span is already on
		// the context here; without one this is simply skipped.
		if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
			builder = builder.
				Str("trace_id", sc.TraceID().String()).
				Str("span_id", sc.SpanID().String())
		}
		lg := builder.Logger()
		c.Request = c.Request.WithContext(logging.Into(ctx, lg))

		c.Next()

		lg.Info().
			Int("status", c.Writer.Status()).
			Int("bytes", c.Writer.Size()).
			Msg("request")
	}
}

// requireToken enforces a bearer token on mutating routes. It is a no-op when
// no token is configured, which Config only allows in development.
func (s *Server) requireToken() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.token == "" {
			c.Next()
			return
		}
		const prefix = "Bearer "
		got := c.GetHeader("Authorization")
		presented, ok := strings.CutPrefix(got, prefix)
		// Constant-time compare so a timing signal cannot leak the token
		// prefix-by-prefix to an attacker who can measure response latency.
		if !ok || subtle.ConstantTimeCompare([]byte(presented), []byte(s.token)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorBody{Error: "unauthorized"})
			return
		}
		c.Next()
	}
}

// StatusBody is returned by the liveness and readiness probes.
type StatusBody struct {
	Status string `json:"status"`
}

// VersionBody reports build provenance.
type VersionBody struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

func (s *Server) health(c *gin.Context) {
	c.JSON(http.StatusOK, StatusBody{Status: "ok"})
}

// ready reports readiness to serve traffic. It touches the store, so a broken
// dependency surfaces here rather than on the first real request.
func (s *Server) ready(c *gin.Context) {
	if _, err := s.store.List(c.Request.Context()); err != nil {
		lg := logging.From(c.Request.Context())
		lg.Error().Err(err).Msg("readiness probe failed")
		c.JSON(http.StatusServiceUnavailable, StatusBody{Status: "unavailable"})
		return
	}
	c.JSON(http.StatusOK, StatusBody{Status: "ready"})
}

func (s *Server) version(c *gin.Context) {
	c.JSON(http.StatusOK, VersionBody{Version: config.Version, Commit: config.Commit})
}

func (s *Server) listNotes(c *gin.Context) {
	out, err := s.store.List(c.Request.Context())
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) getNote(c *gin.Context) {
	n, err := s.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, n)
}

func (s *Server) createNote(c *gin.Context) {
	var d notes.Draft
	if err := c.ShouldBindJSON(&d); err != nil {
		c.JSON(http.StatusBadRequest, ErrorBody{Error: "request body is not valid JSON"})
		return
	}
	n, err := s.store.Create(c.Request.Context(), d)
	if err != nil {
		s.fail(c, err)
		return
	}
	c.Header("Location", "/v1/notes/"+n.ID)
	c.JSON(http.StatusCreated, n)
}

func (s *Server) deleteNote(c *gin.Context) {
	if err := s.store.Delete(c.Request.Context(), c.Param("id")); err != nil {
		s.fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// fail maps a domain error onto a status code. Unrecognised errors become 500
// and are logged, never echoed: an internal message must not reach a client.
func (s *Server) fail(c *gin.Context, err error) {
	switch {
	case errors.Is(err, notes.ErrNotFound):
		c.JSON(http.StatusNotFound, ErrorBody{Error: "not found"})
	case errors.Is(err, notes.ErrInvalid):
		c.JSON(http.StatusBadRequest, ErrorBody{Error: err.Error()})
	default:
		lg := logging.From(c.Request.Context())
		lg.Error().Err(err).Msg("unhandled error")
		c.JSON(http.StatusInternalServerError, ErrorBody{Error: "internal error"})
	}
}
