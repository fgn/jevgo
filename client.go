package jev

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// SDK defaults, matching the official TypeSafe SDKs.
const (
	DefaultBaseURL = "https://api.typesafe.ai"
	DefaultModel   = "jev-latest"
	// DefaultTimeout is the per-attempt timeout; retries are not bounded by
	// it. See [RetryPolicy.TotalTimeout] for a whole-call budget.
	DefaultTimeout = 10 * time.Second
)

// Environment variables read by [NewClient] when the matching option is not
// set. Empty or whitespace-only values are ignored.
const (
	EnvAPIKey       = "TYPESAFE_API_KEY"
	EnvBaseURL      = "TYPESAFE_BASE_URL"
	EnvDefaultModel = "TYPESAFE_DEFAULT_MODEL"
	EnvLogLevel     = "TYPESAFE_LOG_LEVEL"
)

const (
	systemOnePath = "/v1/systemone"
	modelsPath    = "/v1/models"
)

// ErrInvalidConfig is wrapped by every configuration error returned from
// [NewClient] or from applying an [Option].
var ErrInvalidConfig = errors.New("jev: invalid configuration")

// Client calls the TypeSafe API. It is safe for concurrent use; construct it
// once with [NewClient] and share it.
type Client struct {
	cfg config
}

type config struct {
	apiKey       string
	baseURL      string
	defaultModel string
	timeout      time.Duration
	retry        *RetryPolicy
	headers      http.Header
	httpClient   *http.Client
	logger       *slog.Logger
	tracer       Tracer
}

// Option configures a [Client]. Options apply in order with last-wins
// precedence. Every option is also accepted per call by [Client.SystemOne]
// and [Client.ListModels], where it overrides the client setting for that
// call only.
type Option func(*config) error

// WithAPIKey sets the API key. Defaults to TYPESAFE_API_KEY.
func WithAPIKey(key string) Option {
	return func(c *config) error {
		c.apiKey = strings.TrimSpace(key)
		return nil
	}
}

// WithBaseURL sets the API root, for example for a proxy. Trailing slashes
// are removed. Defaults to TYPESAFE_BASE_URL, then [DefaultBaseURL].
func WithBaseURL(baseURL string) Option {
	return func(c *config) error {
		c.baseURL = strings.TrimSpace(baseURL)
		return nil
	}
}

// WithDefaultModel sets the model used when [Request.Model] is empty.
// Defaults to TYPESAFE_DEFAULT_MODEL, then [DefaultModel].
func WithDefaultModel(model string) Option {
	return func(c *config) error {
		c.defaultModel = strings.TrimSpace(model)
		return nil
	}
}

// WithHTTPClient sets the HTTP client used for requests. This is the place
// to install an instrumented or customized transport. A nil client selects
// [http.DefaultClient]. Per-attempt timeouts are applied through the request
// context, so the client's own Timeout is not required.
func WithHTTPClient(client *http.Client) Option {
	return func(c *config) error {
		c.httpClient = client
		return nil
	}
}

// WithTimeout sets the timeout for each HTTP attempt, including reading the
// full response body. It must be positive. Defaults to [DefaultTimeout].
func WithTimeout(timeout time.Duration) Option {
	return func(c *config) error {
		if timeout <= 0 {
			return fmt.Errorf("%w: timeout must be positive, got %v", ErrInvalidConfig, timeout)
		}
		c.timeout = timeout
		return nil
	}
}

// WithRetryPolicy replaces the retry policy. Start from
// [DefaultRetryPolicy] and adjust fields; a zero RetryPolicy disables
// retries.
func WithRetryPolicy(policy RetryPolicy) Option {
	return func(c *config) error {
		err := policy.validate()
		if err != nil {
			return err
		}
		policy.Statuses = append([]int(nil), policy.Statuses...)
		c.retry = &policy
		return nil
	}
}

// WithHeader adds a header sent with every request. Authorization, Accept,
// Content-Type, and the SDK identification headers cannot be overridden.
func WithHeader(key, value string) Option {
	return func(c *config) error {
		if c.headers == nil {
			c.headers = http.Header{}
		}
		c.headers.Add(key, value)
		return nil
	}
}

// WithHeaders adds headers sent with every request; see [WithHeader].
func WithHeaders(headers http.Header) Option {
	return func(c *config) error {
		for key, values := range headers {
			for _, value := range values {
				err := WithHeader(key, value)(c)
				if err != nil {
					return err
				}
			}
		}
		return nil
	}
}

// WithLogger sets the logger. The SDK logs one summary record per HTTP
// attempt at [slog.LevelInfo] and request and response headers and bodies
// at [slog.LevelDebug]. Credential headers are redacted; bodies are not.
// Without a logger, TYPESAFE_LOG_LEVEL (debug, info, warn, error, or off)
// selects a level on [slog.Default]; unset means no logging.
func WithLogger(logger *slog.Logger) Option {
	return func(c *config) error {
		c.logger = logger
		return nil
	}
}

// WithTracer sets the [Tracer] that observes SystemOne calls. Compose several
// with [MultiTracer]. A nil tracer disables tracing.
func WithTracer(tracer Tracer) Option {
	return func(c *config) error {
		c.tracer = tracer
		return nil
	}
}

// NewClient creates a client. Explicit options take precedence over
// environment variables, which take precedence over SDK defaults. The API
// key is required.
func NewClient(opts ...Option) (*Client, error) {
	var cfg config
	err := cfg.apply(opts)
	if err != nil {
		return nil, err
	}
	err = cfg.resolve()
	if err != nil {
		return nil, err
	}
	return &Client{cfg: cfg}, nil
}

// BaseURL returns the configured API root without a trailing slash.
func (c *Client) BaseURL() string { return c.cfg.baseURL }

// DefaultModel returns the model used when a request does not name one.
func (c *Client) DefaultModel() string { return c.cfg.defaultModel }

// SystemOne evaluates req.State against req.Questions and returns one answer
// per question under the same names.
//
// Errors are [*APIError] for unsuccessful responses after retries,
// [*ConnectionError] for transport failures and per-attempt timeouts after
// retries, [*ResponseValidationError] for undecodable successful responses,
// an error wrapping [ErrInvalidRequest] for requests rejected before
// sending, and the context's error when ctx is done.
func (c *Client) SystemOne(ctx context.Context, req Request, opts ...Option) (*Response, error) {
	cfg, err := c.cfg.with(opts)
	if err != nil {
		return nil, err
	}
	model := req.Model
	if model == "" {
		model = cfg.defaultModel
	}
	body, err := req.marshal(model)
	if err != nil {
		return nil, err
	}

	if cfg.tracer != nil {
		ctx = cfg.tracer.TraceSystemOneStart(ctx, SystemOneStartData{Request: &req, Model: model})
	}
	res, err := cfg.do(ctx, http.MethodPost, systemOnePath, body)
	var resp *Response
	if err == nil {
		resp, err = decodeSystemOne(res)
	}
	if cfg.tracer != nil {
		cfg.tracer.TraceSystemOneEnd(ctx, SystemOneEndData{Response: resp, Err: err, Attempts: res.attempts})
	}
	return resp, err
}

// ListModels returns the models available to the account.
func (c *Client) ListModels(ctx context.Context, opts ...Option) (*ModelsResponse, error) {
	cfg, err := c.cfg.with(opts)
	if err != nil {
		return nil, err
	}
	res, err := cfg.do(ctx, http.MethodGet, modelsPath, nil)
	if err != nil {
		return nil, err
	}
	return decodeModels(res)
}

func (c *config) apply(opts []Option) error {
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		err := opt(c)
		if err != nil {
			return err
		}
	}
	return nil
}

// with returns a copy of the configuration with per-call options applied.
func (c config) with(opts []Option) (config, error) {
	if len(opts) == 0 {
		return c, nil
	}
	c.headers = c.headers.Clone()
	err := c.apply(opts)
	if err != nil {
		return config{}, err
	}
	err = c.resolve()
	if err != nil {
		return config{}, err
	}
	return c, nil
}

// resolve fills unset fields from the environment and SDK defaults, then
// validates the result. It is idempotent.
func (c *config) resolve() error {
	if c.apiKey == "" {
		c.apiKey = env(EnvAPIKey)
	}
	if c.apiKey == "" {
		return fmt.Errorf("%w: no API key: pass WithAPIKey or set %s", ErrInvalidConfig, EnvAPIKey)
	}
	if c.baseURL == "" {
		c.baseURL = env(EnvBaseURL)
	}
	if c.baseURL == "" {
		c.baseURL = DefaultBaseURL
	}
	c.baseURL = strings.TrimRight(c.baseURL, "/")
	parsed, err := url.Parse(c.baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("%w: invalid base URL %q", ErrInvalidConfig, c.baseURL)
	}
	if c.defaultModel == "" {
		c.defaultModel = env(EnvDefaultModel)
	}
	if c.defaultModel == "" {
		c.defaultModel = DefaultModel
	}
	if c.timeout == 0 {
		c.timeout = DefaultTimeout
	}
	if c.retry == nil {
		policy := DefaultRetryPolicy()
		c.retry = &policy
	}
	if c.httpClient == nil {
		c.httpClient = http.DefaultClient
	}
	if c.logger == nil {
		logger, err := loggerFromEnv()
		if err != nil {
			return err
		}
		c.logger = logger
	}
	return nil
}

func env(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}
