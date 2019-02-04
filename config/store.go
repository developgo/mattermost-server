package config

import (
	"github.com/mattermost/mattermost-server/model"
)

// Listener is a callback function invoked when the configuration changes.
type Listener func(oldConfig *model.Config, newConfig *model.Config)

// Store abstracts the act of getting and setting the configuration.
type Store interface {
	// Get fetches the current, cached configuration.
	Get() *model.Config

	// GetEnvironmentOverrides fetches the configuration fields overridden by environment variables.
	GetEnvironmentOverrides() map[string]interface{}

	// Set replaces the current configuration in its entirety, updating the backing store.
	Set(*model.Config) (*model.Config, error)

	// Patch merges the given config with the current configuration, updating the backing store.
	Patch(*model.Config) (*model.Config, error)

	// Load updates the current configuration from the backing store, possibly initializing.
	Load() (needsSave bool, err error)

	// Save writes the current configuration to the backing store.
	//
	// It should not normally be necessary to call this method, as Set and Patch will effect
	// same. However, some initialization may occur on Load that requires a save, but to avoid
	// unexpected writes there, Save is exposed to make this explicit.
	Save() error

	// AddListener adds a callback function to invoke when the configuration is modified.
	AddListener(listener Listener) string

	// RemoveListener removes a callback function using an id returned from AddListener.
	RemoveListener(id string)

	// String describes the backing store for the config.
	String() string

	// Close cleans up resources associated with the store.
	Close() error
}
