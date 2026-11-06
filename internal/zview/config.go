// Package zview is the zview-server data layer: its configuration (design
// §8.2, which amends ui-spec §3.6) and the lazy per-day clip index
// (ui-spec §3.5).
package zview

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/apikdech/zrelay-v2/pkg/secretref"
	"github.com/apikdech/zrelay-v2/pkg/storage"
)

// Config is the zview-server configuration, field for field as design §8.2.
type Config struct {
	Version int           `yaml:"version"`
	Server  ServerConfig  `yaml:"server"`
	Storage StorageConfig `yaml:"storage"`
	Index   IndexConfig   `yaml:"index"`
	Live    LiveConfig    `yaml:"live"`

	AllowLiteralSecrets bool `yaml:"allow_literal_secrets"`
}

// ServerConfig is the HTTP server block. After LoadConfig, APITokenSHA256
// holds resolved hex hashes (one or two, for key rotation).
type ServerConfig struct {
	Listen         string   `yaml:"listen"`
	APITokenSHA256 []string `yaml:"api_token_sha256"`
	AppDir         string   `yaml:"app_dir"`
}

// StorageConfig selects the read-only storage driver. Only the block of the
// selected driver is resolved and validated.
type StorageConfig struct {
	Driver string      `yaml:"driver"` // s3 | local (tests)
	Root   string      `yaml:"root"`
	S3     S3Config    `yaml:"s3"`
	Local  LocalConfig `yaml:"local"`
}

// S3Config is the s3 driver block, with read-only credentials. After
// LoadConfig, Endpoint, AccessKey and SecretKey hold resolved values.
type S3Config struct {
	Endpoint     string `yaml:"endpoint"`
	Region       string `yaml:"region"`
	Bucket       string `yaml:"bucket"`
	UsePathStyle bool   `yaml:"use_path_style"`
	AccessKey    string `yaml:"access_key"`
	SecretKey    string `yaml:"secret_key"`
}

// LocalConfig is the local driver block (tests only).
type LocalConfig struct {
	BaseDir string `yaml:"base_dir"`
}

// IndexConfig tunes the day index (ui-spec §3.5, design §6.7). A zero
// HotRefreshSeconds, ColdCacheDays or AssumedMaxClipSeconds passed to NewIndex
// selects the default; a zero RetentionDays disables retention invalidation.
type IndexConfig struct {
	HotRefreshSeconds     int      `yaml:"hot_refresh_seconds"`
	ColdCacheDays         int      `yaml:"cold_cache_days"`
	AssumedMaxClipSeconds int      `yaml:"assumed_max_clip_seconds"`
	RetentionDays         int      `yaml:"retention_days"`
	CamerasFallback       []string `yaml:"cameras_fallback"`
}

// LiveConfig is the live proxy block.
type LiveConfig struct {
	Upstream string `yaml:"upstream"`
}

// Defaults (design §8.2).
const (
	DefaultHotRefreshSeconds     = 30
	DefaultColdCacheDays         = 60
	DefaultAssumedMaxClipSeconds = 300
	DefaultRetentionDays         = 30
	DefaultLiveUpstream          = "http://zrelay:9465"

	maxAPITokenHashes = 2
)

var cameraIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

func newDefaultConfig() *Config {
	return &Config{
		Index: IndexConfig{
			HotRefreshSeconds:     DefaultHotRefreshSeconds,
			ColdCacheDays:         DefaultColdCacheDays,
			AssumedMaxClipSeconds: DefaultAssumedMaxClipSeconds,
			RetentionDays:         DefaultRetentionDays,
		},
		Live: LiveConfig{Upstream: DefaultLiveUpstream},
	}
}

// LoadConfig reads the zview-server config at path. It decodes strictly (an
// unknown key is an error naming its path), applies defaults, resolves ENV:
// and FILE: references (pkg/secretref) and validates. Errors never contain
// secret values.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("zview config: %w", err)
	}
	cfg, err := parseConfig(data)
	if err != nil {
		return nil, fmt.Errorf("zview config %s: %w", path, err)
	}
	if err := cfg.resolve(); err != nil {
		return nil, fmt.Errorf("zview config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("zview config %s: %w", path, err)
	}
	return cfg, nil
}

func parseConfig(data []byte) (*Config, error) {
	cfg := newDefaultConfig()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty file")
		}
		var te *yaml.TypeError
		if errors.As(err, &te) {
			return nil, unknownKeyPaths(data, te)
		}
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one YAML document")
	}
	return cfg, nil
}

var unknownFieldRE = regexp.MustCompile(`^line (\d+): field (\S+) not found in type `)

// unknownKeyPaths rewrites yaml's "line N: field X not found in type T" into
// "a.b.X: unknown key (line N)". Other messages pass through.
func unknownKeyPaths(data []byte, te *yaml.TypeError) error {
	var root yaml.Node
	_ = yaml.Unmarshal(data, &root)
	errs := make([]error, 0, len(te.Errors))
	for _, msg := range te.Errors {
		m := unknownFieldRE.FindStringSubmatch(msg)
		if m == nil {
			errs = append(errs, errors.New(msg))
			continue
		}
		line, _ := strconv.Atoi(m[1])
		p := findKey(&root, "", line, m[2])
		if p == "" {
			p = m[2]
		}
		errs = append(errs, fmt.Errorf("%s: unknown key (line %d)", p, line))
	}
	return errors.Join(errs...)
}

func findKey(n *yaml.Node, prefix string, line int, key string) string {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			if p := findKey(c, prefix, line, key); p != "" {
				return p
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			p := k.Value
			if prefix != "" {
				p = prefix + "." + k.Value
			}
			if k.Line == line && k.Value == key {
				return p
			}
			if r := findKey(v, p, line, key); r != "" {
				return r
			}
		}
	}
	return ""
}

// resolve resolves every reference in place. Secrets accept a literal only
// when AllowLiteralSecrets is set; the endpoint and token hashes are not
// secrets and always accept literals. All errors are returned together.
func (c *Config) resolve() error {
	var errs []error
	one := func(path string, ref *string, allowLiteral bool) {
		if *ref == "" {
			return // Validate reports a required field
		}
		v, err := secretref.Resolve(*ref, allowLiteral)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			return
		}
		*ref = v
	}
	for i := range c.Server.APITokenSHA256 {
		one(fmt.Sprintf("server.api_token_sha256[%d]", i), &c.Server.APITokenSHA256[i], true)
	}
	if c.Storage.Driver == "s3" {
		one("storage.s3.endpoint", &c.Storage.S3.Endpoint, true)
		one("storage.s3.access_key", &c.Storage.S3.AccessKey, c.AllowLiteralSecrets)
		one("storage.s3.secret_key", &c.Storage.S3.SecretKey, c.AllowLiteralSecrets)
	}
	return errors.Join(errs...)
}

// Validate checks a loaded and resolved Config. Every problem is reported,
// prefixed with its field path. Values that may be secret are never echoed.
func (c *Config) Validate() error {
	var errs []error
	addf := func(path, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", path, fmt.Sprintf(format, args...)))
	}
	positive := func(path string, n int) {
		if n <= 0 {
			addf(path, "must be > 0, got %d", n)
		}
	}

	if c.Version != 2 {
		addf("version", "must be 2, got %d", c.Version)
	}

	if _, _, err := net.SplitHostPort(c.Server.Listen); err != nil {
		addf("server.listen", "must be host:port")
	}
	switch n := len(c.Server.APITokenSHA256); {
	case n < 1 || n > maxAPITokenHashes:
		addf("server.api_token_sha256", "want 1 to %d hashes, got %d", maxAPITokenHashes, n)
	default:
		for i, h := range c.Server.APITokenSHA256 {
			if !isHexSHA256(h) {
				addf(fmt.Sprintf("server.api_token_sha256[%d]", i), "must be 64 hex characters")
			}
		}
	}

	if c.Storage.Root != "" {
		if _, err := storage.CleanKey(c.Storage.Root); err != nil {
			addf("storage.root", "invalid key prefix")
		}
	}
	switch c.Storage.Driver {
	case "s3":
		s3 := c.Storage.S3
		if s3.Endpoint != "" {
			if u, err := url.Parse(s3.Endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				addf("storage.s3.endpoint", "must be an http(s) URL")
			} else if u.User != nil {
				addf("storage.s3.endpoint", "must not carry userinfo")
			}
		} else {
			addf("storage.s3.endpoint", "is required")
		}
		for _, f := range [][2]string{
			{"storage.s3.bucket", s3.Bucket},
			{"storage.s3.access_key", s3.AccessKey},
			{"storage.s3.secret_key", s3.SecretKey},
		} {
			if f[1] == "" {
				addf(f[0], "is required")
			}
		}
	case "local":
		if c.Storage.Local.BaseDir == "" {
			addf("storage.local.base_dir", "is required")
		}
	default:
		addf("storage.driver", "must be s3 or local, got %q", c.Storage.Driver)
	}

	positive("index.hot_refresh_seconds", c.Index.HotRefreshSeconds)
	positive("index.cold_cache_days", c.Index.ColdCacheDays)
	positive("index.assumed_max_clip_seconds", c.Index.AssumedMaxClipSeconds)
	if c.Index.RetentionDays < 0 {
		addf("index.retention_days", "must be >= 0, got %d", c.Index.RetentionDays)
	}
	for i, id := range c.Index.CamerasFallback {
		if !cameraIDRE.MatchString(id) {
			addf(fmt.Sprintf("index.cameras_fallback[%d]", i), "must match %s", cameraIDRE)
		}
	}

	if u, err := url.Parse(c.Live.Upstream); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		addf("live.upstream", "must be an http(s) URL without userinfo")
	}
	return errors.Join(errs...)
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	return strings.Trim(s, "0123456789abcdefABCDEF") == ""
}
