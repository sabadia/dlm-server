package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/posflag"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
	"github.com/spf13/pflag"
)

// EnvPrefix prefixes every environment variable read by Load. Keys map to
// variables by upper-casing and replacing "." with "_", e.g. server.address
// is DLM_SERVER_ADDRESS and log.max_size_mb is DLM_LOG_MAX_SIZE_MB.
const EnvPrefix = "DLM_"

// EnvConfigFile names the environment variable selecting the config file.
const EnvConfigFile = EnvPrefix + "CONFIG"

// configFileName is looked up in the config directory when no file is given.
const configFileName = "config.yaml"

// flagConfig is the flag selecting the config file.
const flagConfig = "config"

// listKeys are keys whose environment values are comma-separated lists.
var listKeys = map[string]bool{"server.allowed_origins": true}

// flagKeys maps the flags registered by BindFlags to configuration keys.
var flagKeys = map[string]string{
	"home":         "home",
	"address":      "server.address",
	"allow-remote": "server.allow_remote",
	"tls-cert":     "server.tls_cert",
	"tls-key":      "server.tls_key",
	"config-dir":   "paths.config_dir",
	"data-dir":     "paths.data_dir",
	"cache-dir":    "paths.cache_dir",
	"log-dir":      "paths.log_dir",
	"log-level":    "log.level",
	"log-format":   "log.format",
	"log-stderr":   "log.stderr",
}

// BindFlags registers the boot-configuration flags on fs. Flag defaults are
// informational only: a flag overrides other sources only when it is set.
func BindFlags(fs *pflag.FlagSet) {
	d := Default()
	fs.String(flagConfig, "", "path to a YAML config file (env "+EnvConfigFile+")")
	fs.String("home", "", "place all dlm directories below this directory (portable mode)")
	fs.String("address", d.Server.Address, "listen address host:port")
	fs.Bool("allow-remote", false, "allow binding a non-loopback address (requires TLS)")
	fs.String("tls-cert", "", "TLS certificate file")
	fs.String("tls-key", "", "TLS private key file")
	fs.String("config-dir", "", "configuration directory")
	fs.String("data-dir", "", "data directory (database, state)")
	fs.String("cache-dir", "", "cache directory")
	fs.String("log-dir", "", "log directory")
	fs.String("log-level", d.Log.Level, "log level: debug, info, warn, error")
	fs.String("log-format", d.Log.Format, "log format: json, text")
	fs.Bool("log-stderr", d.Log.Stderr, "also write logs to stderr")
}

// LoadOptions controls where Load reads configuration from.
type LoadOptions struct {
	// ConfigFile is an explicit config file; it must exist. When empty the
	// --config flag, then DLM_CONFIG, then <config dir>/config.yaml (optional)
	// are consulted.
	ConfigFile string
	// Flags is a flag set populated by BindFlags and already parsed. Optional.
	Flags *pflag.FlagSet
	// Environ returns the environment; defaults to os.Environ.
	Environ func() []string
}

// Load builds the configuration from defaults, config file, environment and
// flags (later sources win), resolves directories and validates the result.
func Load(opts LoadOptions) (*Config, error) {
	if opts.Environ == nil {
		opts.Environ = os.Environ
	}
	k := koanf.New(".")
	if err := k.Load(structs.Provider(Default(), "koanf"), nil); err != nil {
		return nil, fmt.Errorf("config: load defaults: %w", err)
	}
	knownKeys := k.Keys()

	// Environment and flags are loaded into a separate instance first so the
	// config-file location can honour --home/--config-dir/DLM_HOME.
	overrides := koanf.New(".")
	if err := loadEnv(overrides, knownKeys, opts.Environ); err != nil {
		return nil, err
	}
	if err := loadFlags(overrides, opts.Flags); err != nil {
		return nil, err
	}

	path, required, err := configFilePath(opts, overrides)
	if err != nil {
		return nil, err
	}
	if path != "" {
		if err := loadFile(k, path, required); err != nil {
			return nil, err
		}
	}
	if err := k.Merge(overrides); err != nil {
		return nil, fmt.Errorf("config: merge overrides: %w", err)
	}

	cfg := &Config{}
	if err := k.Unmarshal("", cfg); err != nil {
		return nil, fmt.Errorf("config: decode: %w", err)
	}
	if err := cfg.resolvePaths(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func loadEnv(k *koanf.Koanf, knownKeys []string, environ func() []string) error {
	byEnvName := make(map[string]string, len(knownKeys))
	for _, key := range knownKeys {
		byEnvName[EnvPrefix+strings.ToUpper(strings.ReplaceAll(key, ".", "_"))] = key
	}
	provider := env.Provider(".", env.Opt{
		Prefix:      EnvPrefix,
		EnvironFunc: environ,
		TransformFunc: func(name, value string) (string, any) {
			key, ok := byEnvName[name]
			if !ok {
				return "", nil // unknown variable: ignore
			}
			if listKeys[key] {
				return key, splitList(value)
			}
			return key, value
		},
	})
	if err := k.Load(provider, nil); err != nil {
		return fmt.Errorf("config: load environment: %w", err)
	}
	return nil
}

func loadFlags(k *koanf.Koanf, fs *pflag.FlagSet) error {
	if fs == nil {
		return nil
	}
	provider := posflag.ProviderWithFlag(fs, ".", nil, func(f *pflag.Flag) (string, any) {
		key, ok := flagKeys[f.Name]
		if !ok || !f.Changed {
			return "", nil
		}
		return key, posflag.FlagVal(fs, f)
	})
	if err := k.Load(provider, nil); err != nil {
		return fmt.Errorf("config: load flags: %w", err)
	}
	return nil
}

// configFilePath returns the config file to load and whether it must exist.
func configFilePath(opts LoadOptions, overrides *koanf.Koanf) (path string, required bool, err error) {
	if opts.ConfigFile != "" {
		return opts.ConfigFile, true, nil
	}
	if opts.Flags != nil {
		if f := opts.Flags.Lookup(flagConfig); f != nil && f.Changed {
			return f.Value.String(), true, nil
		}
	}
	for _, kv := range opts.Environ() {
		if v, ok := strings.CutPrefix(kv, EnvConfigFile+"="); ok && v != "" {
			return v, true, nil
		}
	}
	// Optional file in the (possibly overridden) config directory.
	probe := Config{Home: overrides.String("home"), Paths: PathsConfig{ConfigDir: overrides.String("paths.config_dir")}}
	if err := probe.resolvePaths(); err != nil {
		return "", false, err
	}
	return filepath.Join(probe.Paths.ConfigDir, configFileName), false, nil
}

func loadFile(k *koanf.Koanf, path string, required bool) error {
	if _, err := os.Stat(path); err != nil {
		if !required && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("config: config file: %w", err)
	}
	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		return fmt.Errorf("config: load %q: %w", path, err)
	}
	return nil
}

// splitList splits a comma-separated value, trimming blanks.
func splitList(v string) []string {
	var out []string
	for item := range strings.SplitSeq(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
