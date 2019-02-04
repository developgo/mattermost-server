package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"

	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-server/mlog"
	"github.com/mattermost/mattermost-server/model"
	"github.com/mattermost/mattermost-server/utils/fileutils"
)

var (
	ReadOnlyConfigurationError = errors.New("configuration is read-only")
)

// fileStore is a config store backed by a file such as config/config.json.
type fileStore struct {
	emitter

	config               *model.Config
	environmentOverrides map[string]interface{}
	configLock           sync.RWMutex
	path                 string
	watch                bool
	watcher              *watcher
	needsSave            bool
}

// NewFileStore creates a new instance of a config store backed by the given file path.
//
// If watch is true, any external changes to the file will force a reload.
func NewFileStore(path string, watch bool) (fs *fileStore, needsSave bool, err error) {
	resolvedPath, err := resolveConfigFilePath(path)
	if err != nil {
		return nil, false, err
	}

	fs = &fileStore{
		path:  resolvedPath,
		watch: watch,
	}
	if _, err = fs.Load(); err != nil {
		return nil, false, err
	}

	if fs.watch {
		if err = fs.startWatcher(); err != nil {
			mlog.Error("failed to start config watcher", mlog.String("path", path), mlog.Err(err))
		}
	}

	return fs, fs.needsSave, nil
}

// resolveConfigFilePath attempts to resolve the given configuration file path to an absolute path.
//
// Consideration is given to maintaining backwards compatibility when resolving the path to the
// configuration file.
func resolveConfigFilePath(path string) (string, error) {
	// Absolute paths are explicit and require no resolution.
	if filepath.IsAbs(path) {
		return path, nil
	}

	// Search for the given relative path (or plain filename) in various directories,
	// resolving to the corresponding absolute path if found. FindConfigFile takes into account
	// various common search paths rooted both at the current working directory and relative
	// to the executable.
	if configFile := fileutils.FindConfigFile(path); configFile != "" {
		return configFile, nil
	}

	// Otherwise, search for the config/ folder using the same heuristics as above, and build
	// an absolute path anchored there and joining the given input path (or plain filename).
	if configFolder, found := fileutils.FindDir("config"); found {
		return filepath.Join(configFolder, path), nil
	}

	// Fail altogether if we can't even find the config/ folder. This should only happen if
	// the executable is relocated away from the supporting files.
	return "", fmt.Errorf("failed to find config file %s", path)
}

// Get fetches the current, cached configuration.
func (fs *fileStore) Get() *model.Config {
	fs.configLock.RLock()
	defer fs.configLock.RUnlock()

	return fs.config
}

// GetEnvironmentOverrides fetches the configuration fields overridden by environment variables.
func (fs *fileStore) GetEnvironmentOverrides() map[string]interface{} {
	fs.configLock.RLock()
	defer fs.configLock.RUnlock()

	return fs.environmentOverrides
}

// Set replaces the current configuration in its entirety.
func (fs *fileStore) Set(newCfg *model.Config) (*model.Config, error) {
	fs.configLock.Lock()
	var unlockOnce sync.Once
	defer func() {
		unlockOnce.Do(func() {
			fs.configLock.Unlock()
		})
	}()

	oldCfg := fs.config

	// Disallow attempting to save a directly modified config (comparing pointers). This is
	// not an exhaustive check, given the use of pointers throughout the data structure, but
	// prevents the common case.
	if newCfg == oldCfg {
		return nil, errors.New("old configuration modified instead of cloning")
	}

	newCfg.SetDefaults()

	// Sometimes the config is received with "fake" data in sensitive fields. Apply the real
	// data from the existing config as necessary.
	desanitize(oldCfg, newCfg)

	if err := newCfg.IsValid(); err != nil {
		return nil, errors.Wrap(err, "new configuration is invalid")
	}

	if *oldCfg.ClusterSettings.Enable && *oldCfg.ClusterSettings.ReadOnlyConfig {
		return nil, ReadOnlyConfigurationError
	}

	if err := fs.persist(newCfg); err != nil {
		return nil, errors.Wrap(err, "failed to persist")
	}

	fs.config = newCfg

	unlockOnce.Do(func() {
		fs.configLock.Unlock()
	})

	// Notify listeners synchronously. Ideally, this would be asynchronous, but existing code
	// assumes this and there would be increased complexity to avoid racing updates.
	fs.invokeConfigListeners(oldCfg, newCfg)

	return oldCfg, nil
}

// Patch merges the given config with the current configuration.
func (fs *fileStore) Patch(*model.Config) (*model.Config, error) {
	fs.configLock.Lock()
	defer fs.configLock.Unlock()

	panic("patch is not yet implemented")

	return fs.config, nil
}

// serialize converts the given configuration into JSON bytes for persistence.
func (fs *fileStore) serialize(cfg *model.Config) ([]byte, error) {
	return json.MarshalIndent(cfg, "", "    ")
}

// persist writes the configuration to the configured file.
func (fs *fileStore) persist(cfg *model.Config) error {
	fs.stopWatcher()

	b, err := fs.serialize(cfg)
	if err != nil {
		return errors.Wrap(err, "failed to serialize")
	}

	err = ioutil.WriteFile(fs.path, b, 0644)
	if err != nil {
		return errors.Wrap(err, "failed to write file")
	}

	fs.needsSave = false
	if fs.watch {
		if err = fs.startWatcher(); err != nil {
			mlog.Error("failed to start config watcher", mlog.String("path", fs.path), mlog.Err(err))
		}
	}

	return nil
}

// Load updates the current configuration from the backing store.
func (fs *fileStore) Load() (needsSave bool, err error) {
	var f io.ReadCloser

	f, err = os.Open(fs.path)
	if os.IsNotExist(err) {
		needsSave = true
		defaultCfg := model.Config{}
		defaultCfg.SetDefaults()

		var defaultCfgBytes []byte
		defaultCfgBytes, err = fs.serialize(&defaultCfg)
		if err != nil {
			return false, errors.Wrap(err, "failed to serialize default config")
		}

		f = ioutil.NopCloser(bytes.NewReader(defaultCfgBytes))

	} else if err != nil {
		return false, errors.Wrapf(err, "failed to open %s for reading", fs.path)
	}
	defer func() {
		closeErr := f.Close()
		if err == nil && closeErr != nil {
			err = errors.Wrap(closeErr, "failed to close")
		}
	}()

	allowEnvironmentOverrides := true
	loadedCfg, environmentOverrides, err := readConfig(f, allowEnvironmentOverrides)
	if err != nil {
		return false, errors.Wrapf(err, "failed to load config from %s", fs.path)
	}

	// SetDefaults generates various keys and salts if not previously configured. Determine if
	// such a change will be made before invoking. This method will not effect the save: that
	// remains the responsibility of the caller.
	needsSave = needsSave || loadedCfg.SqlSettings.AtRestEncryptKey == nil || len(*loadedCfg.SqlSettings.AtRestEncryptKey) == 0
	needsSave = needsSave || loadedCfg.FileSettings.PublicLinkSalt == nil || len(*loadedCfg.FileSettings.PublicLinkSalt) == 0
	needsSave = needsSave || loadedCfg.EmailSettings.InviteSalt == nil || len(*loadedCfg.EmailSettings.InviteSalt) == 0

	loadedCfg.SetDefaults()

	if err := loadedCfg.IsValid(); err != nil {
		return false, errors.Wrap(err, "invalid config")
	}

	if changed := fixConfig(loadedCfg); changed {
		needsSave = true
	}

	fs.configLock.Lock()

	oldCfg := fs.config
	fs.needsSave = needsSave
	fs.config = loadedCfg
	fs.environmentOverrides = environmentOverrides

	fs.configLock.Unlock()

	// Notify listeners synchronously. Ideally, this would be asynchronous, but existing code
	// assumes this and there would be increased complexity to avoid racing updates.
	fs.invokeConfigListeners(oldCfg, loadedCfg)

	return fs.needsSave, nil
}

// Save writes the current configuration to the backing store.
func (fs *fileStore) Save() error {
	fs.configLock.RLock()
	defer fs.configLock.RUnlock()

	return fs.persist(fs.config)
}

// startWatcher starts a watcher to monitor for external config file changes.
func (fs *fileStore) startWatcher() error {
	if fs.watcher != nil {
		return nil
	}

	watcher, err := newWatcher(fs.path, func() {
		if _, err := fs.Load(); err != nil {
			mlog.Error("failed to reload file on change", mlog.String("path", fs.path), mlog.Err(err))
		}
	})
	if err != nil {
		return err
	}

	fs.watcher = watcher

	return nil
}

// stopWatcher stops any previously started watcher.
func (fs *fileStore) stopWatcher() {
	if fs.watcher == nil {
		return
	}

	if err := fs.watcher.Close(); err != nil {
		mlog.Error("failed to close watcher", mlog.Err(err))
	}
	fs.watcher = nil
}

// String returns the path to the file backing the config.
func (fs *fileStore) String() string {
	return "file://" + fs.path
}

// Close cleans up resources associated with the store.
func (fs *fileStore) Close() error {
	fs.stopWatcher()

	return nil
}
