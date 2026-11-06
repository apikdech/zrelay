// Package config loads the zrelay configuration file (design §8.1, which
// amends relay-spec §5.2–§5.3).
//
// Load parses strictly (an unknown key is an error, which is how the removed
// smb, sftp, retention and mlock keys are rejected), applies defaults,
// resolves ENV: and FILE: references (pkg/secretref) and runs Validate.
// Errors name the offending field path, such as "cameras[1].id", and never
// contain secret values.
package config

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
	"time"

	"go.yaml.in/yaml/v3"
)

// Config is the zrelay configuration, field for field as design §8.1.
type Config struct {
	Version             int            `yaml:"version"`
	Relay               Relay          `yaml:"relay"`
	Encryption          Encryption     `yaml:"encryption"`
	Storage             Storage        `yaml:"storage"`
	Live                Live           `yaml:"live"`
	Restream            Restream       `yaml:"restream"`
	Defaults            CameraDefaults `yaml:"defaults"`
	Cameras             []Camera       `yaml:"cameras"`
	AllowLiteralSecrets bool           `yaml:"allow_literal_secrets"`
}

// Relay is the relay process block.
type Relay struct {
	ID                   string `yaml:"id"`
	MaxMemoryMB          int    `yaml:"max_memory_mb"`
	EgressBacklogMB      int    `yaml:"egress_backlog_mb"`
	UploadWorkers        int    `yaml:"upload_workers"`
	ShutdownGraceSeconds int    `yaml:"shutdown_grace_seconds"`
	HTTPListen           string `yaml:"http_listen"`
}

// Encryption is the encryption block.
type Encryption struct {
	// MasterKey is the master key reference, kept unresolved.
	MasterKey string `yaml:"master_key"`
	// MasterKeyText is MasterKey resolved to text by Load. It is not parsed
	// or checked here (key parsing belongs to pkg/zrec).
	MasterKeyText string `yaml:"-"`
	ChunkSizeKiB  int    `yaml:"chunk_size_kib"`
}

// Storage is the storage block. Only the block of the selected driver is
// resolved and validated.
type Storage struct {
	Driver  string  `yaml:"driver"`
	Root    string  `yaml:"root"`
	S3      S3      `yaml:"s3"`
	Local   Local   `yaml:"local"`
	Breaker Breaker `yaml:"breaker"`
}

// S3 is the s3 driver block. After Load, Endpoint, AccessKey and SecretKey
// hold resolved values.
type S3 struct {
	Endpoint     string `yaml:"endpoint"`
	Region       string `yaml:"region"`
	Bucket       string `yaml:"bucket"`
	UsePathStyle bool   `yaml:"use_path_style"`
	AccessKey    string `yaml:"access_key"`
	SecretKey    string `yaml:"secret_key"`
	PartSizeMiB  int    `yaml:"part_size_mib"`
}

// Local is the local driver block (tests only).
type Local struct {
	BaseDir string `yaml:"base_dir"`
}

// Breaker is the upload circuit breaker block.
type Breaker struct {
	FailureThreshold int           `yaml:"failure_threshold"`
	BaseDelay        time.Duration `yaml:"base_delay"`
	MaxDelay         time.Duration `yaml:"max_delay"`
}

// Live is the live view block. MaxViewers 0 disables live view; only then
// are AdvertiseIP and APITokenSHA256 left unresolved.
type Live struct {
	Listen         string   `yaml:"listen"`
	AdvertiseIP    string   `yaml:"advertise_ip"`
	ICEUDPPort     int      `yaml:"ice_udp_port"`
	MaxViewers     int      `yaml:"max_viewers"`
	APITokenSHA256 []string `yaml:"api_token_sha256"`
}

// Restream is the RTSP restream block. An empty Listen disables the
// restream; otherwise Username and Password hold resolved values after Load.
type Restream struct {
	Listen   string `yaml:"listen"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// CameraDefaults are inherited by every camera.
type CameraDefaults struct {
	PreRollSeconds        int `yaml:"pre_roll_seconds"`
	PostRollSeconds       int `yaml:"post_roll_seconds"`
	MaxClipSeconds        int `yaml:"max_clip_seconds"`
	MaxBitrateKbps        int `yaml:"max_bitrate_kbps"`
	MaxGOPSeconds         int `yaml:"max_gop_seconds"`
	TriggerTimeoutSeconds int `yaml:"trigger_timeout_seconds"`
	RecorderQueueFrames   int `yaml:"recorder_queue_frames"`
	ViewerQueueFrames     int `yaml:"viewer_queue_frames"`
}

// CameraOverrides are the per-camera overrides of CameraDefaults, written
// at the camera level with the same keys. Nil means inherit.
type CameraOverrides struct {
	PreRollSeconds        *int `yaml:"pre_roll_seconds"`
	PostRollSeconds       *int `yaml:"post_roll_seconds"`
	MaxClipSeconds        *int `yaml:"max_clip_seconds"`
	MaxBitrateKbps        *int `yaml:"max_bitrate_kbps"`
	MaxGOPSeconds         *int `yaml:"max_gop_seconds"`
	TriggerTimeoutSeconds *int `yaml:"trigger_timeout_seconds"`
	RecorderQueueFrames   *int `yaml:"recorder_queue_frames"`
	ViewerQueueFrames     *int `yaml:"viewer_queue_frames"`
}

// Camera is one camera entry. The YAML fields keep what the file says; the
// fields tagged `yaml:"-"` are filled in by Load.
type Camera struct {
	ID          string      `yaml:"id"`
	Name        string      `yaml:"name"`
	Brand       string      `yaml:"brand"`
	Tags        []string    `yaml:"tags"`
	HostRef     string      `yaml:"host"`
	Credentials Credentials `yaml:"credentials"`
	RTSP        RTSP        `yaml:"rtsp"`
	Events      Events      `yaml:"events"`

	Overrides CameraOverrides `yaml:",inline"`

	// Resolved by Load.
	Host     string `yaml:"-"`
	Username string `yaml:"-"`
	Password string `yaml:"-"`

	// EventsPort is events.port resolved to a number.
	EventsPort int `yaml:"-"`

	// Effective values: the camera override, else Defaults.
	PreRoll             time.Duration `yaml:"-"`
	PostRoll            time.Duration `yaml:"-"`
	MaxClip             time.Duration `yaml:"-"`
	MaxBitrateKbps      int           `yaml:"-"`
	MaxGOP              time.Duration `yaml:"-"`
	TriggerTimeout      time.Duration `yaml:"-"`
	RecorderQueueFrames int           `yaml:"-"`
	ViewerQueueFrames   int           `yaml:"-"`

	// Rules are the event rules from events.rules. They stay empty for a
	// tapo camera without rules; the built-in Tapo rules (design §4.3) are
	// supplied by the events package.
	Rules []RuleConfig `yaml:"-"`
}

// Credentials are the camera credential references.
type Credentials struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// RTSP is the camera RTSP source.
type RTSP struct {
	Port int    `yaml:"port"`
	Path string `yaml:"path"`
}

// Events is the camera event source. Port is a literal number or an ENV:
// reference; its resolved value is Camera.EventsPort.
type Events struct {
	Source      string       `yaml:"source"`
	Port        string       `yaml:"port"`
	ServicePath string       `yaml:"service_path"`
	Rules       []RuleConfig `yaml:"rules"`
}

// RuleConfig maps an ONVIF notification to a trigger kind.
type RuleConfig struct {
	Kind        string `yaml:"kind"`
	Topic       string `yaml:"topic"`
	Item        string `yaml:"item"`
	ActiveValue string `yaml:"active_value"`
}

// Defaults (design §8.1, D13).
const (
	DefaultHTTPListen          = "127.0.0.1:9464"
	DefaultMasterKeyRef        = "FILE:/run/secrets/zrelay_master"
	DefaultEventsPort          = 2020
	DefaultRTSPPort            = 554
	DefaultEventsSource        = "onvif"
	DefaultRuleActiveValue     = "true"
	defaultUploadWorkers       = 2
	defaultShutdownGrace       = 30
	defaultChunkSizeKiB        = 1024
	defaultPartSizeMiB         = 8
	defaultFailureThreshold    = 5
	defaultBreakerBaseDelay    = time.Second
	defaultBreakerMaxDelay     = 5 * time.Minute
	defaultPreRollSeconds      = 15
	defaultPostRollSeconds     = 10
	defaultMaxClipSeconds      = 300
	defaultMaxBitrateKbps      = 4000
	defaultMaxGOPSeconds       = 4
	defaultTriggerTimeout      = 60
	defaultRecorderQueueFrames = 256
	defaultViewerQueueFrames   = 64
)

// newDefault returns a Config holding every default. The file is decoded
// over it, so keys the file omits keep their default.
func newDefault() *Config {
	return &Config{
		Relay: Relay{
			UploadWorkers:        defaultUploadWorkers,
			ShutdownGraceSeconds: defaultShutdownGrace,
			HTTPListen:           DefaultHTTPListen,
		},
		Encryption: Encryption{
			MasterKey:    DefaultMasterKeyRef,
			ChunkSizeKiB: defaultChunkSizeKiB,
		},
		Storage: Storage{
			S3: S3{PartSizeMiB: defaultPartSizeMiB},
			Breaker: Breaker{
				FailureThreshold: defaultFailureThreshold,
				BaseDelay:        defaultBreakerBaseDelay,
				MaxDelay:         defaultBreakerMaxDelay,
			},
		},
		Defaults: CameraDefaults{
			PreRollSeconds:        defaultPreRollSeconds,
			PostRollSeconds:       defaultPostRollSeconds,
			MaxClipSeconds:        defaultMaxClipSeconds,
			MaxBitrateKbps:        defaultMaxBitrateKbps,
			MaxGOPSeconds:         defaultMaxGOPSeconds,
			TriggerTimeoutSeconds: defaultTriggerTimeout,
			RecorderQueueFrames:   defaultRecorderQueueFrames,
			ViewerQueueFrames:     defaultViewerQueueFrames,
		},
	}
}

// Load reads the config file at path, decodes it strictly over the
// defaults, resolves references and validates the result.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	cfg, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	cfg.applyCameraDefaults()
	if err := cfg.resolve(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// parse decodes data strictly over the defaults. Unknown-key errors are
// rewritten to name the key's full path.
func parse(data []byte) (*Config, error) {
	cfg := newDefault()
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

// unknownKeyPaths turns yaml's "line N: field X not found in type T"
// messages into "a.b.X: unknown key (line N)". Other messages pass through.
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

// findKey returns the path of the mapping key named key on line, or "".
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
	case yaml.SequenceNode:
		for i, c := range n.Content {
			if p := findKey(c, fmt.Sprintf("%s[%d]", prefix, i), line, key); p != "" {
				return p
			}
		}
	}
	return ""
}

// applyCameraDefaults fills per-camera defaults and effective values.
func (c *Config) applyCameraDefaults() {
	pick := func(o *int, d int) int {
		if o != nil {
			return *o
		}
		return d
	}
	sec := func(n int) time.Duration { return time.Duration(n) * time.Second }
	d := c.Defaults
	for i := range c.Cameras {
		cam := &c.Cameras[i]
		o := cam.Overrides
		if cam.RTSP.Port == 0 {
			cam.RTSP.Port = DefaultRTSPPort
		}
		if cam.Events.Source == "" {
			cam.Events.Source = DefaultEventsSource
		}
		if cam.Events.Port == "" {
			cam.Events.Port = strconv.Itoa(DefaultEventsPort)
		}
		cam.PreRoll = sec(pick(o.PreRollSeconds, d.PreRollSeconds))
		cam.PostRoll = sec(pick(o.PostRollSeconds, d.PostRollSeconds))
		cam.MaxClip = sec(pick(o.MaxClipSeconds, d.MaxClipSeconds))
		cam.MaxBitrateKbps = pick(o.MaxBitrateKbps, d.MaxBitrateKbps)
		cam.MaxGOP = sec(pick(o.MaxGOPSeconds, d.MaxGOPSeconds))
		cam.TriggerTimeout = sec(pick(o.TriggerTimeoutSeconds, d.TriggerTimeoutSeconds))
		cam.RecorderQueueFrames = pick(o.RecorderQueueFrames, d.RecorderQueueFrames)
		cam.ViewerQueueFrames = pick(o.ViewerQueueFrames, d.ViewerQueueFrames)
		cam.Rules = make([]RuleConfig, len(cam.Events.Rules))
		for j, r := range cam.Events.Rules {
			if r.ActiveValue == "" {
				r.ActiveValue = DefaultRuleActiveValue
			}
			cam.Rules[j] = r
		}
	}
}

// RTSPURL returns the camera's RTSP source URL,
// rtsp://user:pass@host:port/path, with the userinfo escaped. The result
// carries credentials and must never be logged.
func (c *Config) RTSPURL(cam Camera) string {
	p, q, _ := strings.Cut(cam.RTSP.Path, "?")
	u := url.URL{
		Scheme:   "rtsp",
		User:     url.UserPassword(cam.Username, cam.Password),
		Host:     net.JoinHostPort(cam.Host, strconv.Itoa(cam.RTSP.Port)),
		Path:     p,
		RawQuery: q,
	}
	return u.String()
}
