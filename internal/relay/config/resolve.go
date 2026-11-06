package config

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/apikdech/zrelay-v2/pkg/secretref"
)

// resolve resolves every reference in place, except Encryption.MasterKey,
// which is kept and resolved into Encryption.MasterKeyText. Secret fields
// accept a literal only when AllowLiteralSecrets is set; the non-secret
// fields that take refs (endpoint, hosts, ports, advertise IP, token hashes)
// always accept literals. Blocks that are not in use (the storage driver
// not selected, live with max_viewers 0, restream without listen) are not
// resolved. All errors are returned together.
func (c *Config) resolve() error {
	var errs []error
	// An empty field is left empty for Validate to report as required.
	secret := func(path string, ref *string) {
		if *ref == "" {
			return
		}
		v, err := secretref.Resolve(*ref, c.AllowLiteralSecrets)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			return
		}
		*ref = v
	}
	plain := func(path string, ref *string) bool {
		if *ref == "" {
			return true
		}
		v, err := secretref.Resolve(*ref, true)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			return false
		}
		*ref = v
		return true
	}

	c.Encryption.MasterKeyText = c.Encryption.MasterKey
	secret("encryption.master_key", &c.Encryption.MasterKeyText)

	if c.Storage.Driver == "s3" {
		s3 := &c.Storage.S3
		plain("storage.s3.endpoint", &s3.Endpoint)
		secret("storage.s3.access_key", &s3.AccessKey)
		secret("storage.s3.secret_key", &s3.SecretKey)
	}

	if c.Live.MaxViewers > 0 {
		plain("live.advertise_ip", &c.Live.AdvertiseIP)
		for i := range c.Live.APITokenSHA256 {
			plain(fmt.Sprintf("live.api_token_sha256[%d]", i), &c.Live.APITokenSHA256[i])
		}
	}

	if c.Restream.Listen != "" {
		secret("restream.username", &c.Restream.Username)
		secret("restream.password", &c.Restream.Password)
	}

	for i := range c.Cameras {
		cam := &c.Cameras[i]
		p := fmt.Sprintf("cameras[%d]", i)
		cam.Host = cam.HostRef
		plain(p+".host", &cam.Host)
		cam.Username = cam.Credentials.Username
		secret(p+".credentials.username", &cam.Username)
		cam.Password = cam.Credentials.Password
		secret(p+".credentials.password", &cam.Password)

		port := cam.Events.Port
		if plain(p+".events.port", &port) {
			n, err := strconv.Atoi(port)
			if err != nil {
				// The value may come from the environment; do not echo it.
				errs = append(errs, fmt.Errorf("%s.events.port: not a number", p))
			} else {
				cam.EventsPort = n
			}
		}
	}
	return errors.Join(errs...)
}
