// Package main implements a tiny reverse proxy that serves the latest
// published version of SwaggerHub APIs as plain Swagger JSON.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/cors"
	"github.com/gofiber/fiber/v3/middleware/logger"
	"gopkg.in/yaml.v3"
)

// -----------------------------------------------------------------------------
// Config
// -----------------------------------------------------------------------------

type Config struct {
	Server struct {
		Port string `yaml:"port"`
	} `yaml:"server"`

	Auth struct {
		APIKey string `yaml:"api_key"`
	} `yaml:"auth"`

	SwaggerHub struct {
		APIKey  string        `yaml:"api_key"`
		Timeout time.Duration `yaml:"timeout"`
	} `yaml:"swaggerhub"`

	Cache struct {
		TTL time.Duration `yaml:"ttl"`
	} `yaml:"cache"`

	APIs map[string]APIRef `yaml:"apis"`
}

type APIRef struct {
	Owner string `yaml:"owner"`
	Name  string `yaml:"name"`
}

const (
	defaultSwaggerHubTimeout = 20 * time.Second
	defaultCacheTTL          = 5 * time.Minute
	swaggerHubErrorMaxLen    = 200
)

func loadConfig(path string) (*Config, error) {
	// filepath.Clean collapses `..` and duplicate separators, giving gosec
	// (G304) a stable, lexically normalized path to analyze. The path itself
	// is operator-supplied via CONFIG_PATH, so no allow-listing is needed —
	// we just want to read exactly what was requested, nothing sneakier.
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err = yaml.Unmarshal([]byte(os.ExpandEnv(string(data))), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Defaults.
	if cfg.Server.Port == "" {
		cfg.Server.Port = "3000"
	}
	if cfg.SwaggerHub.Timeout == 0 {
		cfg.SwaggerHub.Timeout = defaultSwaggerHubTimeout
	}
	if cfg.Cache.TTL == 0 {
		cfg.Cache.TTL = defaultCacheTTL
	}
	if v := os.Getenv("SWAGGERHUB_API_KEY"); v != "" {
		cfg.SwaggerHub.APIKey = v
	}
	if v := os.Getenv("AUTH_API_KEY"); v != "" {
		cfg.Auth.APIKey = v
	}

	if cfg.SwaggerHub.APIKey == "" {
		return nil, errors.New("swaggerhub.api_key (or SWAGGERHUB_API_KEY env) is required")
	}
	if len(cfg.APIs) == 0 {
		return nil, errors.New("at least one entry under `apis` is required")
	}

	return &cfg, nil
}

// -----------------------------------------------------------------------------
// SwaggerHub client
// -----------------------------------------------------------------------------

// swaggerHub is a minimal client for the two endpoints we need.
type swaggerHub struct {
	http   *http.Client
	apiKey string
}

// setAuth attaches both authentication headers SwaggerHub accepts. We send
// the two variants together because private APIs in some SwaggerHub setups
// reject requests that use only one of them.
func (s *swaggerHub) setAuth(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("X-Swaggerhub-Api-Key", s.apiKey)
	req.Header.Set("Accept", "application/json")
}

// pageSize is SwaggerHub's max items per listing page.
const pageSize = 50

// apiListing is the shape we care about from GET /apis/{owner}/{name}.
// Everything else in the payload is intentionally ignored.
type apiListing struct {
	TotalCount int `json:"totalCount"`
	APIs       []struct {
		Properties []apiProperty `json:"properties"`
	} `json:"apis"`
}

type apiProperty struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// revision is a flattened view of a single API revision.
type revision struct {
	version string
	updated time.Time
}

func (r revision) valid() bool { return r.version != "" && !r.updated.IsZero() }

func newRevision(props []apiProperty) revision {
	var r revision
	var created time.Time
	for _, p := range props {
		switch p.Type {
		case "X-Version":
			r.version = p.Value
		case "X-Modified":
			r.updated = parseTime(p.Value)
		case "X-Created":
			created = parseTime(p.Value)
		}
	}
	if r.updated.IsZero() {
		r.updated = created
	}
	return r
}

// latestVersion returns the version string of the most recently updated
// revision, picking the largest X-Modified (or X-Created) timestamp across
// all pages.
func (s *swaggerHub) latestVersion(ctx context.Context, owner, name string) (string, error) {
	var latest revision

	for offset := 0; ; {
		url := fmt.Sprintf(
			"https://api.swaggerhub.com/apis/%s/%s?offset=%d&limit=%d",
			owner, name, offset, pageSize,
		)

		var page apiListing
		if err := s.getJSON(ctx, url, &page); err != nil {
			return "", fmt.Errorf("list versions: %w", err)
		}

		for _, v := range page.APIs {
			r := newRevision(v.Properties)
			if r.valid() && r.updated.After(latest.updated) {
				latest = r
			}
		}

		offset += len(page.APIs)
		if len(page.APIs) == 0 || offset >= page.TotalCount {
			break
		}
	}

	if latest.version == "" {
		return "", errors.New("no versions found")
	}
	return latest.version, nil
}

// spec fetches the Swagger JSON of a specific version as an opaque blob.
// We don't need to decode it — we just forward the bytes.
func (s *swaggerHub) spec(ctx context.Context, owner, name, version string) (json.RawMessage, error) {
	url := fmt.Sprintf("https://api.swaggerhub.com/apis/%s/%s/%s/swagger.json", owner, name, version)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	s.setAuth(req)

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("swaggerhub %d: %s", resp.StatusCode, truncate(string(body), swaggerHubErrorMaxLen))
	}
	return body, nil
}

func (s *swaggerHub) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	s.setAuth(req)

	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("swaggerhub %d: %s", resp.StatusCode, truncate(string(body), swaggerHubErrorMaxLen))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// -----------------------------------------------------------------------------
// Cache
// -----------------------------------------------------------------------------

type cache struct {
	mu      sync.RWMutex
	ttl     time.Duration
	entries map[string]*cacheEntry
}

// cacheEntry holds the JSON spec (always populated) and its YAML rendering
// (lazily filled on the first YAML request within the TTL window).
type cacheEntry struct {
	json    json.RawMessage
	yaml    []byte
	expires time.Time
}

func newCache(ttl time.Duration) *cache {
	return &cache{ttl: ttl, entries: make(map[string]*cacheEntry)}
}

// liveEntry returns the live entry for a key, or nil if absent or expired.
// Caller must already hold the lock appropriate for its operation.
func (c *cache) liveEntry(key string) *cacheEntry {
	e, ok := c.entries[key]
	if !ok || time.Now().After(e.expires) {
		return nil
	}
	return e
}

// getJSON returns the cached JSON spec for a key, if fresh.
func (c *cache) getJSON(key string) (json.RawMessage, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if e := c.liveEntry(key); e != nil {
		return e.json, true
	}
	return nil, false
}

// getYAML returns the cached YAML rendering for a key, if fresh and present.
func (c *cache) getYAML(key string) ([]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if e := c.liveEntry(key); e != nil && e.yaml != nil {
		return e.yaml, true
	}
	return nil, false
}

// setJSON stores the JSON spec for a key, resetting its TTL and dropping
// any previously rendered YAML (it was based on stale data).
func (c *cache) setJSON(key string, data json.RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = &cacheEntry{json: data, expires: time.Now().Add(c.ttl)}
}

// setYAML attaches a YAML rendering to an existing fresh entry. If the entry
// has disappeared (e.g. expired between render start and now), the write is
// skipped.
func (c *cache) setYAML(key string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.liveEntry(key); e != nil {
		e.yaml = data
	}
}

// -----------------------------------------------------------------------------
// HTTP layer
// -----------------------------------------------------------------------------

// apiKeyAuth returns a middleware that requires the X-API-Key request header
// to match the configured key. Comparison is constant-time to avoid leaking
// the key through response timing.
func apiKeyAuth(key string) fiber.Handler {
	expected := []byte(key)
	return func(c fiber.Ctx) error {
		got := []byte(c.Get("X-API-Key"))
		if subtle.ConstantTimeCompare(got, expected) != 1 {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
		}
		return c.Next()
	}
}

type server struct {
	cfg   *Config
	hub   *swaggerHub
	cache *cache
	log   *slog.Logger
}

// fetchSpec returns the latest JSON spec for `key`, using the cache when
// fresh and hitting SwaggerHub otherwise.
func (s *server) fetchSpec(ctx context.Context, key string) (json.RawMessage, error) {
	if spec, ok := s.cache.getJSON(key); ok {
		return spec, nil
	}

	api, ok := s.cfg.APIs[key]
	if !ok {
		return nil, errUnknownAPI
	}

	version, err := s.hub.latestVersion(ctx, api.Owner, api.Name)
	if err != nil {
		return nil, fmt.Errorf("resolve version: %w", err)
	}

	spec, err := s.hub.spec(ctx, api.Owner, api.Name, version)
	if err != nil {
		return nil, fmt.Errorf("fetch spec: %w", err)
	}

	s.log.InfoContext(ctx, "fetched", "api", key, "version", version, "bytes", len(spec))
	s.cache.setJSON(key, spec)
	return spec, nil
}

// errUnknownAPI is returned by fetchSpec for unknown alias lookups so the
// HTTP layer can map it to a 404.
var errUnknownAPI = errors.New("unknown api")

func (s *server) handleJSON(c fiber.Ctx) error {
	key := c.Params("apiKey")

	ctx, cancel := context.WithTimeout(c.Context(), s.cfg.SwaggerHub.Timeout)
	defer cancel()

	spec, err := s.fetchSpec(ctx, key)
	if err != nil {
		return s.writeError(c, key, err)
	}
	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	return c.Send(spec)
}

func (s *server) handleYAML(c fiber.Ctx) error {
	key := c.Params("apiKey")

	if out, ok := s.cache.getYAML(key); ok {
		c.Set(fiber.HeaderContentType, "application/yaml; charset=utf-8")
		return c.Send(out)
	}

	ctx, cancel := context.WithTimeout(c.Context(), s.cfg.SwaggerHub.Timeout)
	defer cancel()

	spec, err := s.fetchSpec(ctx, key)
	if err != nil {
		return s.writeError(c, key, err)
	}

	out, err := jsonToYAML(spec)
	if err != nil {
		s.log.Error("render yaml", "api", key, "err", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	s.cache.setYAML(key, out)

	c.Set(fiber.HeaderContentType, "application/yaml; charset=utf-8")
	return c.Send(out)
}

func (s *server) writeError(c fiber.Ctx, key string, err error) error {
	if errors.Is(err, errUnknownAPI) {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
	}
	s.log.Error("serve", "api", key, "err", err)
	return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": err.Error()})
}

// jsonToYAML re-encodes a JSON document as YAML. It decodes into any so the
// structure survives the round-trip exactly.
func jsonToYAML(data json.RawMessage) ([]byte, error) {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}
	out, err := yaml.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode yaml: %w", err)
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// -----------------------------------------------------------------------------
// Entrypoint
// -----------------------------------------------------------------------------

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "./config.yml"
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		log.Error("load config", "path", configPath, "err", err)
		os.Exit(1)
	}

	srv := &server{
		cfg:   cfg,
		cache: newCache(cfg.Cache.TTL),
		log:   log,
		hub: &swaggerHub{
			http:   &http.Client{Timeout: cfg.SwaggerHub.Timeout},
			apiKey: cfg.SwaggerHub.APIKey,
		},
	}

	app := fiber.New(fiber.Config{AppName: "swaggerhub-latest-proxy"})
	app.Use(logger.New())
	app.Use(cors.New())

	if cfg.Auth.APIKey != "" {
		app.Use("/swagger", apiKeyAuth(cfg.Auth.APIKey))
		log.Info("api key auth enabled")
	}

	app.Get("/swagger/:apiKey.json", srv.handleJSON)
	app.Get("/swagger/:apiKey.yaml", srv.handleYAML)
	app.Get("/swagger/:apiKey.yml", srv.handleYAML)
	app.Get("/healthz", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})

	// Graceful shutdown.
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		log.Info("shutting down")
		_ = app.Shutdown()
	}()

	log.Info("listening",
		"addr", ":"+cfg.Server.Port,
		"config", configPath,
		"apis", len(cfg.APIs),
	)

	if err = app.Listen(":" + cfg.Server.Port); err != nil {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}
