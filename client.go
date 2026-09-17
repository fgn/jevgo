package jev

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

// Defaults, matching the official TypeSafe SDKs.
const (
	DefaultBaseURL = "https://api.typesafe.ai"
	DefaultModel   = "jev-latest"
	// DefaultTimeout applies to each HTTP attempt; see
	// [RetryPolicy.TotalTimeout] for a whole-call deadline.
	DefaultTimeout = 10 * time.Second
)

// Environment variables read when the matching option is not set. Blank
// values are ignored.
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

// ErrInvalidConfig is wrapped by configuration errors.
var ErrInvalidConfig = errors.New("jev: invalid configuration")

// Client calls the TypeSafe API. Create it with [NewClient]; it is safe for
// concurrent use.
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

// Option configures a [Client]. Options apply in order, last wins, and are
// also accepted per call by [Client.SystemOne] and [Client.ListModels] as
// overrides for that call. An empty string or nil value means "not set" and
// falls back to the environment or the default.
type Option func(*config) error

// WithAPIKey sets the API key. Defaults to TYPESAFE_API_KEY.
func WithAPIKey(key string) Option {
	return func(c *config) error {
		c.apiKey = strings.TrimSpace(key)
		return nil
	}
}

// WithBaseURL sets the API root, for example a proxy. It must be an http or
// https URL without credentials, query, or fragment. Defaults to
// TYPESAFE_BASE_URL, then [DefaultBaseURL].
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

// WithHTTPClient sets the HTTP client, which is where to install a custom or
// instrumented transport. Defaults to [http.DefaultClient]. Per-attempt
// timeouts are applied through the request context; a Timeout on the client
// also applies and is reported as a transport timeout.
func WithHTTPClient(client *http.Client) Option {
	return func(c *config) error {
		c.httpClient = client
		return nil
	}
}

// WithTimeout sets the timeout for each HTTP attempt, including reading the
// response body. Defaults to [DefaultTimeout].
func WithTimeout(timeout time.Duration) Option {
	return func(c *config) error {
		if timeout <= 0 {
			return fmt.Errorf("%w: timeout must be positive, got %v", ErrInvalidConfig, timeout)
		}
		c.timeout = timeout
		return nil
	}
}

// WithRetryPolicy replaces the retry policy.
func WithRetryPolicy(policy RetryPolicy) Option {
	policy = policy.clone()
	return func(c *config) error {
		err := policy.validate()
		if err != nil {
			return err
		}
		applied := policy.clone()
		c.retry = &applied
		return nil
	}
}

// WithMaxRetries changes only the retry count, keeping the rest of the
// current policy. 0 disables retries.
func WithMaxRetries(n int) Option {
	return func(c *config) error {
		if n < 0 {
			return fmt.Errorf("%w: retries must not be negative, got %d", ErrInvalidConfig, n)
		}
		policy := DefaultRetryPolicy()
		if c.retry != nil {
			policy = c.retry.clone()
		}
		policy.MaxRetries = n
		c.retry = &policy
		return nil
	}
}

// WithHeader sets a header sent with every request, replacing any earlier
// value. Authorization, Accept, Content-Type, and the SDK identification
// headers cannot be overridden.
func WithHeader(key, value string) Option {
	return WithHeaders(http.Header{key: {value}})
}

// WithHeaders sets headers sent with every request; each key replaces any
// earlier values for that key.
func WithHeaders(headers http.Header) Option {
	return func(c *config) error {
		if c.headers == nil {
			c.headers = http.Header{}
		}
		for key, values := range headers {
			c.headers[http.CanonicalHeaderKey(key)] = slices.Clone(values)
		}
		return nil
	}
}

// WithLogger sets the logger: one record per HTTP attempt at
// [slog.LevelInfo], and headers and bodies at [slog.LevelDebug]. Credential
// headers are redacted; bodies, which contain your state and answers, are
// not. Without a logger, TYPESAFE_LOG_LEVEL (debug, info, warn, error, or
// off) selects a level on [slog.Default]; unset means no logging.
func WithLogger(logger *slog.Logger) Option {
	return func(c *config) error {
		c.logger = logger
		return nil
	}
}

// WithTracer sets the [Tracer] observing SystemOne calls; nil disables it.
func WithTracer(tracer Tracer) Option {
	return func(c *config) error {
		c.tracer = tracer
		return nil
	}
}

// NewClient creates a client. Explicit options take precedence over
// environment variables, which take precedence over defaults. The API key
// is required.
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
// Errors are [*APIError] for unsuccessful responses, [*ConnectionError] for
// transport failures and timeouts, both after retries;
// [*ResponseValidationError] for a successful response that does not match
// the contract; an error wrapping [ErrInvalidRequest] for requests rejected
// before sending; and the context's error when ctx is done.
func (c *Client) SystemOne(ctx context.Context, req Request, opts ...Option) (*Response, error) {
	cfg, err := c.cfg.with(opts)
	if err != nil {
		return nil, err
	}
	encoded, err := req.encode(cfg.defaultModel)
	if err != nil {
		return nil, err
	}
	if cfg.tracer != nil {
		ctx = cfg.tracer.TraceSystemOneStart(ctx, SystemOneStartData{Request: &req, Model: encoded.model, Body: encoded.body})
	}
	res, err := cfg.do(ctx, http.MethodPost, systemOnePath, encoded.body)
	var resp *Response
	if err == nil {
		resp, err = decodeSystemOne(res, encoded.specs)
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

// with returns a copy with per-call options applied.
func (c config) with(opts []Option) (config, error) {
	if c.httpClient == nil {
		return config{}, fmt.Errorf("%w: Client must be created with NewClient", ErrInvalidConfig)
	}
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

// resolve fills unset fields from the environment and defaults, then
// validates. It is idempotent.
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
	baseURL, err := parseBaseURL(c.baseURL)
	if err != nil {
		return err
	}
	c.baseURL = baseURL
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
		c.logger, err = loggerFromEnv()
	}
	return err
}

// parseBaseURL rejects anything that could leak into logs and errors or
// break path concatenation, without echoing the offending value.
func parseBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: invalid base URL", ErrInvalidConfig)
	}
	switch {
	case parsed.Scheme != "http" && parsed.Scheme != "https":
		return "", fmt.Errorf("%w: base URL scheme must be http or https", ErrInvalidConfig)
	case parsed.Host == "":
		return "", fmt.Errorf("%w: base URL needs a host", ErrInvalidConfig)
	case parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery:
		return "", fmt.Errorf("%w: base URL must not contain credentials, a query, or a fragment", ErrInvalidConfig)
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func env(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}
