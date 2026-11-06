package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Bounds (Contract §4.1, design §8.1).
const (
	minChunkSizeKiB   = 64
	maxChunkSizeKiB   = 16384
	minPartSizeMiB    = 5
	maxAPITokenHashes = 2
)

var cameraIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Validate checks a loaded and resolved Config (relay-spec §5.3 as amended
// by design §8.1). Every problem is reported, each prefixed with its field
// path. Values that may be secret or carry userinfo are never echoed.
func (c *Config) Validate() error {
	v := &validator{}

	if c.Version != 2 {
		v.addf("version", "must be 2, got %d", c.Version)
	}
	c.validateRelay(v)
	c.validateEncryption(v)
	c.validateStorage(v)
	c.validateLive(v)
	c.validateRestream(v)
	c.validateCameras(v)
	return errors.Join(v.errs...)
}

type validator struct{ errs []error }

func (v *validator) addf(path, format string, args ...any) {
	v.errs = append(v.errs, fmt.Errorf("%s: %s", path, fmt.Sprintf(format, args...)))
}

func (v *validator) required(path, value string) {
	if value == "" {
		v.addf(path, "is required")
	}
}

func (v *validator) positive(path string, n int) {
	if n <= 0 {
		v.addf(path, "must be > 0, got %d", n)
	}
}

func (v *validator) port(path string, n int) {
	if n < 1 || n > 65535 {
		v.addf(path, "must be a port in [1, 65535], got %d", n)
	}
}

func (v *validator) listen(path, addr string) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		v.addf(path, "must be host:port")
	}
}

func (c *Config) validateRelay(v *validator) {
	r := c.Relay
	v.required("relay.id", r.ID)
	v.positive("relay.max_memory_mb", r.MaxMemoryMB)
	v.positive("relay.egress_backlog_mb", r.EgressBacklogMB)
	v.positive("relay.upload_workers", r.UploadWorkers)
	if r.ShutdownGraceSeconds < 0 {
		v.addf("relay.shutdown_grace_seconds", "must be >= 0, got %d", r.ShutdownGraceSeconds)
	}
	v.listen("relay.http_listen", r.HTTPListen)
}

func (c *Config) validateEncryption(v *validator) {
	e := c.Encryption
	v.required("encryption.master_key", e.MasterKeyText)
	n := e.ChunkSizeKiB
	if n < minChunkSizeKiB || n > maxChunkSizeKiB || n&(n-1) != 0 {
		v.addf("encryption.chunk_size_kib", "must be a power of two in [%d, %d], got %d",
			minChunkSizeKiB, maxChunkSizeKiB, n)
	}
}

func (c *Config) validateStorage(v *validator) {
	s := c.Storage
	switch s.Driver {
	case "s3":
		validateEndpoint(v, s.S3.Endpoint)
		v.required("storage.s3.bucket", s.S3.Bucket)
		v.required("storage.s3.access_key", s.S3.AccessKey)
		v.required("storage.s3.secret_key", s.S3.SecretKey)
		if s.S3.PartSizeMiB < minPartSizeMiB {
			v.addf("storage.s3.part_size_mib", "must be >= %d, got %d", minPartSizeMiB, s.S3.PartSizeMiB)
		}
	case "local":
		v.required("storage.local.base_dir", s.Local.BaseDir)
		if rootRepeated(s.Local.BaseDir, s.Root) {
			v.addf("storage.local.base_dir", "must not end with storage.root %q (Contract §7.1)", s.Root)
		}
	default:
		v.addf("storage.driver", "must be s3 or local, got %q", s.Driver)
	}

	b := s.Breaker
	v.positive("storage.breaker.failure_threshold", b.FailureThreshold)
	if b.BaseDelay <= 0 {
		v.addf("storage.breaker.base_delay", "must be > 0, got %v", b.BaseDelay)
	}
	if b.MaxDelay <= 0 {
		v.addf("storage.breaker.max_delay", "must be > 0, got %v", b.MaxDelay)
	}
	if b.BaseDelay > b.MaxDelay {
		v.addf("storage.breaker.base_delay", "%v exceeds storage.breaker.max_delay %v", b.BaseDelay, b.MaxDelay)
	}
}

// validateEndpoint requires an absolute http(s) URL. The value is not
// echoed because it may carry userinfo.
func validateEndpoint(v *validator, endpoint string) {
	const path = "storage.s3.endpoint"
	if endpoint == "" {
		v.required(path, endpoint)
		return
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		v.addf(path, "must be an absolute http:// or https:// URL")
		return
	}
	if u.User != nil {
		v.addf(path, "must not contain userinfo; use storage.s3.access_key and secret_key")
	}
}

// rootRepeated reports whether the trailing path elements of baseDir are
// root (Contract §7.1).
func rootRepeated(baseDir, root string) bool {
	root = strings.Trim(filepath.ToSlash(root), "/")
	if root == "" || baseDir == "" {
		return false
	}
	base := filepath.ToSlash(filepath.Clean(baseDir))
	return base == root || strings.HasSuffix(base, "/"+root)
}

func (c *Config) validateLive(v *validator) {
	l := c.Live
	if l.MaxViewers < 0 {
		v.addf("live.max_viewers", "must be >= 0 (0 disables live), got %d", l.MaxViewers)
	}
	if l.MaxViewers <= 0 {
		return
	}
	v.listen("live.listen", l.Listen)
	if net.ParseIP(l.AdvertiseIP) == nil {
		v.addf("live.advertise_ip", "must be an IP address")
	}
	v.port("live.ice_udp_port", l.ICEUDPPort)
	if n := len(l.APITokenSHA256); n < 1 || n > maxAPITokenHashes {
		v.addf("live.api_token_sha256", "must hold 1 to %d hashes, got %d", maxAPITokenHashes, n)
	}
	for i, h := range l.APITokenSHA256 {
		if b, err := hex.DecodeString(h); err != nil || len(b) != 32 {
			v.addf(fmt.Sprintf("live.api_token_sha256[%d]", i), "must be a 64-digit hex SHA-256")
		}
	}
}

func (c *Config) validateRestream(v *validator) {
	r := c.Restream
	if r.Listen == "" {
		return
	}
	v.listen("restream.listen", r.Listen)
	v.required("restream.username", r.Username)
	v.required("restream.password", r.Password)
}

func (c *Config) validateCameras(v *validator) {
	seen := make(map[string]bool, len(c.Cameras))
	for i, cam := range c.Cameras {
		p := fmt.Sprintf("cameras[%d]", i)
		switch {
		case !cameraIDRE.MatchString(cam.ID):
			v.addf(p+".id", "%q does not match %s", cam.ID, cameraIDRE)
		case seen[cam.ID]:
			v.addf(p+".id", "duplicate camera id %q", cam.ID)
		}
		seen[cam.ID] = true

		v.required(p+".host", cam.Host)
		v.required(p+".credentials.username", cam.Username)
		v.required(p+".credentials.password", cam.Password)
		v.port(p+".rtsp.port", cam.RTSP.Port)
		if !strings.HasPrefix(cam.RTSP.Path, "/") {
			v.addf(p+".rtsp.path", "must start with /")
		}
		if cam.Events.Source != "onvif" {
			v.addf(p+".events.source", "must be onvif, got %q", cam.Events.Source)
		}
		v.port(p+".events.port", cam.EventsPort)
		validateRules(v, p, cam)
		validateEffective(v, p, cam)
	}
}

// probeTopics are matched to detect a match-all topic pattern.
var probeTopics = []string{"", "x", "tns1:RuleEngine/CellMotionDetector/Motion"}

func validateRules(v *validator, p string, cam Camera) {
	if len(cam.Rules) == 0 && cam.Brand != "tapo" {
		v.addf(p+".events.rules", "are required unless brand is tapo (built-in rules)")
	}
	for j, r := range cam.Rules {
		rp := fmt.Sprintf("%s.events.rules[%d]", p, j)
		v.required(rp+".kind", r.Kind)
		if r.Topic == "" {
			v.required(rp+".topic", r.Topic)
			continue
		}
		re, err := regexp.Compile(r.Topic)
		if err != nil {
			v.addf(rp+".topic", "invalid pattern: %v", err)
			continue
		}
		if r.Item == "" && slices.IndexFunc(probeTopics, func(s string) bool { return !re.MatchString(s) }) < 0 {
			v.addf(rp+".item", "is required when topic %q matches every topic", r.Topic)
		}
	}
}

// validateEffective checks the per-camera values after defaults merged.
func validateEffective(v *validator, p string, cam Camera) {
	nonNeg := func(key string, d time.Duration) {
		if d < 0 {
			v.addf(p+"."+key, "must be >= 0, got %v", d)
		}
	}
	pos := func(key string, d time.Duration) {
		if d <= 0 {
			v.addf(p+"."+key, "must be > 0, got %v", d)
		}
	}
	nonNeg("pre_roll_seconds", cam.PreRoll)
	nonNeg("post_roll_seconds", cam.PostRoll)
	pos("max_clip_seconds", cam.MaxClip)
	pos("max_gop_seconds", cam.MaxGOP)
	pos("trigger_timeout_seconds", cam.TriggerTimeout)
	v.positive(p+".max_bitrate_kbps", cam.MaxBitrateKbps)
	v.positive(p+".recorder_queue_frames", cam.RecorderQueueFrames)
	v.positive(p+".viewer_queue_frames", cam.ViewerQueueFrames)
}
