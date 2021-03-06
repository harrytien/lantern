package config

import (
	"crypto/x509"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/getlantern/appdir"
	"github.com/getlantern/detour"
	"github.com/getlantern/fronted"
	"github.com/getlantern/golog"
	"github.com/getlantern/keyman"
	"github.com/getlantern/proxiedsites"
	"github.com/getlantern/yaml"
	"github.com/getlantern/yamlconf"

	"github.com/getlantern/flashlight/client"
	"github.com/getlantern/flashlight/proxied"
)

const (
	cloudfront = "cloudfront"

	// DefaultUpdateServerURL is the URL to fetch updates from.
	DefaultUpdateServerURL = "https://update.getlantern.org"
)

var (
	log = golog.LoggerFor("flashlight.config")
	m   *yamlconf.Manager
	r   = regexp.MustCompile("\\d+\\.\\d+")
)

// Config contains general configuration for Lantern either set globally via
// the cloud, in command line flags, or in local customizations during
// development.
type Config struct {
	configDir       string
	Version         int
	CloudConfigCA   string
	CPUProfile      string
	MemProfile      string
	UpdateServerURL string
	Client          *client.ClientConfig
	ProxiedSites    *proxiedsites.Config // List of proxied site domains that get routed through Lantern rather than accessed directly
	TrustedCAs      []*fronted.CA
}

// StartPolling starts the process of polling for new configuration files.
func StartPolling() {
	// Force detour to whitelist chained domain
	u, err := url.Parse(defaultChainedCloudConfigURL)
	if err != nil {
		log.Fatalf("Unable to parse chained cloud config URL: %v", err)
	}
	log.Debugf("Polling at %v", defaultChainedCloudConfigURL)
	detour.ForceWhitelist(u.Host)

	// No-op if already started.
	m.StartPolling()
}

// validateConfig checks whether the given config is valid and returns an error
// if it isn't.
func validateConfig(_cfg yamlconf.Config) error {
	cfg, ok := _cfg.(*Config)
	if !ok {
		return fmt.Errorf("Config is not a flashlight config!")
	}

	nc := len(cfg.Client.ChainedServers)

	log.Debugf("Found %v chained servers in config on disk", nc)
	for _, v := range cfg.Client.ChainedServers {
		log.Debugf("chained server: %v", v)
	}
	// The config will have more than one but fewer than 10 chained servers
	// if it has been given a custom config with a custom chained server
	// list
	if nc <= 0 || nc > 10 {
		return fmt.Errorf("Inappropriate number of custom chained servers found: %d", nc)
	}
	return nil
}

func majorVersion(version string) string {
	return r.FindString(version)
}

// Init initializes the configuration system.
//
// version - the version of lantern
// stickyConfig - if true, we ignore cloud updates
// flags - map of flags (generally from command-line) that always get applied
//         to the config.
func Init(userConfig UserConfig, version string, configDir string, stickyConfig bool, flags map[string]interface{}) (*Config, error) {
	// Request the config via either chained servers or direct fronted servers.
	cf := proxied.ParallelPreferChained()
	fetcher := NewFetcher(userConfig, cf, flags)

	file := "lantern-" + version + ".yaml"
	_, configPath, err := inConfigDir(configDir, file)
	if err != nil {
		log.Errorf("Could not get config path? %v", err)
		return nil, err
	}

	m = &yamlconf.Manager{
		FilePath: configPath,

		ValidateConfig: validateConfig,

		DefaultConfig: MakeInitialConfig,

		EmptyConfig: func() yamlconf.Config {
			return &Config{configDir: configDir}
		},
		PerSessionSetup: func(ycfg yamlconf.Config) error {
			cfg := ycfg.(*Config)
			return cfg.applyFlags(flags)
		},
		CustomPoll: func(ycfg yamlconf.Config) (mutate func(yamlconf.Config) error, waitTime time.Duration, err error) {
			return fetcher.pollForConfig(ycfg, stickyConfig)
		},
		// Obfuscate on-disk contents of YAML file
		Obfuscate: flags["readableconfig"] == nil || !flags["readableconfig"].(bool),
	}
	initial, err := m.Init()

	var cfg *Config
	if err != nil {
		log.Errorf("Error initializing config: %v", err)
	} else {
		cfg = initial.(*Config)
	}
	log.Debug("Returning config")
	return cfg, err
}

// Run runs the configuration system.
func Run(updateHandler func(updated *Config)) error {
	for {
		next := m.Next()
		nextCfg := next.(*Config)
		updateHandler(nextCfg)
	}
}

// Update updates the configuration using the given mutator function.
func Update(mutate func(cfg *Config) error) error {
	return m.Update(func(ycfg yamlconf.Config) error {
		return mutate(ycfg.(*Config))
	})
}

func inConfigDir(configDir string, filename string) (string, string, error) {
	cdir := configDir

	if cdir == "" {
		cdir = appdir.General("Lantern")
	}

	log.Debugf("Using config dir %v", cdir)
	if _, err := os.Stat(cdir); err != nil {
		if os.IsNotExist(err) {
			// Create config dir
			if err := os.MkdirAll(cdir, 0750); err != nil {
				return "", "", fmt.Errorf("Unable to create configdir at %s: %s", cdir, err)
			}
		}
	}

	return cdir, filepath.Join(cdir, filename), nil
}

func (cfg *Config) GetTrustedCACerts() (pool *x509.CertPool, err error) {
	certs := make([]string, 0, len(cfg.TrustedCAs))
	for _, ca := range cfg.TrustedCAs {
		certs = append(certs, ca.Cert)
	}
	pool, err = keyman.PoolContainingCerts(certs...)
	if err != nil {
		log.Errorf("Could not create pool %v", err)
	}
	return
}

// GetVersion implements the method from interface yamlconf.Config
func (cfg *Config) GetVersion() int {
	return cfg.Version
}

// SetVersion implements the method from interface yamlconf.Config
func (cfg *Config) SetVersion(version int) {
	cfg.Version = version
}

// applyFlags updates this Config from any command-line flags that were passed
// in.
func (cfg *Config) applyFlags(flags map[string]interface{}) error {
	if cfg.Client == nil {
		cfg.Client = &client.ClientConfig{}
	}

	var visitErr error

	// Visit all flags that have been set and copy to config
	for key, value := range flags {
		switch key {
		// General
		case "cloudconfigca":
			cfg.CloudConfigCA = value.(string)
		case "cpuprofile":
			cfg.CPUProfile = value.(string)
		case "memprofile":
			cfg.MemProfile = value.(string)
		}
	}
	if visitErr != nil {
		return visitErr
	}

	return nil
}

// ApplyDefaults implements the method from interface yamlconf.Config
//
// ApplyDefaults populates default values on a Config to make sure that we have
// a minimum viable config for running.  As new settings are added to
// flashlight, this function should be updated to provide sensible defaults for
// those settings.
func (cfg *Config) ApplyDefaults() {
	if cfg.UpdateServerURL == "" {
		cfg.UpdateServerURL = "https://update.getlantern.org"
	}

	if cfg.Client == nil {
		cfg.Client = &client.ClientConfig{}
	}

	cfg.applyClientDefaults()

	if cfg.ProxiedSites == nil {
		log.Debugf("Adding empty proxiedsites")
		cfg.ProxiedSites = &proxiedsites.Config{
			Delta: &proxiedsites.Delta{
				Additions: []string{},
				Deletions: []string{},
			},
			Cloud: []string{},
		}
	}

	if cfg.ProxiedSites.Cloud == nil || len(cfg.ProxiedSites.Cloud) == 0 {
		log.Debugf("Loading default cloud proxiedsites")
		cfg.ProxiedSites.Cloud = defaultProxiedSites
	}

	if cfg.TrustedCAs == nil || len(cfg.TrustedCAs) == 0 {
		cfg.TrustedCAs = fronted.DefaultTrustedCAs
	}
}

func (cfg *Config) applyClientDefaults() {
	// Make sure we always have at least one masquerade set
	if cfg.Client.MasqueradeSets == nil {
		cfg.Client.MasqueradeSets = make(map[string][]*fronted.Masquerade)
	}
	if len(cfg.Client.MasqueradeSets) == 0 {
		cfg.Client.MasqueradeSets[cloudfront] = fronted.DefaultCloudfrontMasquerades
	}

	// Always make sure we have a map of ChainedServers
	if cfg.Client.ChainedServers == nil {
		cfg.Client.ChainedServers = make(map[string]*client.ChainedServerInfo)
	}

	// Make sure we always have at least one server
	if len(cfg.Client.ChainedServers) == 0 {
		cfg.Client.ChainedServers = make(map[string]*client.ChainedServerInfo, len(fallbacks))
		for key, fb := range fallbacks {
			cfg.Client.ChainedServers[key] = fb
		}
	}

	if cfg.Client.ProxiedCONNECTPorts == nil {
		cfg.Client.ProxiedCONNECTPorts = []int{
			// Standard HTTP(S) ports
			80, 443,
			// Common unprivileged HTTP(S) ports
			8080, 8443,
			// XMPP
			5222, 5223, 5224,
			// Android
			5228, 5229,
			// udpgw
			7300,
			// Google Hangouts TCP Ports (see https://support.google.com/a/answer/1279090?hl=en)
			19305, 19306, 19307, 19308, 19309,
		}
	}
}

// updateFrom creates a new Config by 'merging' the given yaml into this Config.
// The masquerade sets, the collections of servers, and the trusted CAs in the
// update yaml  completely replace the ones in the original Config.
func (cfg *Config) updateFrom(updateBytes []byte) error {
	// XXX: does this need a mutex, along with everyone that uses the config?
	oldChainedServers := cfg.Client.ChainedServers
	oldMasqueradeSets := cfg.Client.MasqueradeSets
	oldTrustedCAs := cfg.TrustedCAs
	cfg.Client.ChainedServers = map[string]*client.ChainedServerInfo{}
	cfg.Client.MasqueradeSets = map[string][]*fronted.Masquerade{}
	cfg.TrustedCAs = []*fronted.CA{}
	err := yaml.Unmarshal(updateBytes, cfg)
	if err != nil {
		cfg.Client.ChainedServers = oldChainedServers
		cfg.Client.MasqueradeSets = oldMasqueradeSets
		cfg.TrustedCAs = oldTrustedCAs
		return fmt.Errorf("Unable to unmarshal YAML for update: %s", err)
	}
	// Deduplicate global proxiedsites
	if len(cfg.ProxiedSites.Cloud) > 0 {
		wlDomains := make(map[string]bool)
		for _, domain := range cfg.ProxiedSites.Cloud {
			wlDomains[domain] = true
		}
		cfg.ProxiedSites.Cloud = make([]string, 0, len(wlDomains))
		for domain := range wlDomains {
			cfg.ProxiedSites.Cloud = append(cfg.ProxiedSites.Cloud, domain)
		}
		sort.Strings(cfg.ProxiedSites.Cloud)
	}
	return nil
}
