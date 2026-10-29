package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// secretsFileName holds the credentials the panel writes: one entry per named
// provider, next to config.json and never inside it. The configuration is read
// by the panel on every page load and is copied into bug reports, so a key must
// not travel with it.
const secretsFileName = "secrets.json"

// maxAPIKeyBytes bounds one credential. Real keys are well under this; the cap
// stops a paste of a whole file from reaching the panel body or the audit line.
const maxAPIKeyBytes = 1024

// providerSecrets is the on-disk shape of secrets.json.
type providerSecrets struct {
	Providers map[string]string `json:"providers"`
}

// secretStore reads and writes secrets.json. It keeps no memory cache: a role
// rebuild is rare, and re-reading a small file is cheaper than serving a stale
// key after the operator saves a new one.
type secretStore struct {
	path string
	warn sync.Once
}

func newSecretStore(configPath string) *secretStore {
	dir := filepath.Dir(configPath)
	if configPath == "" {
		dir = "."
	}
	return &secretStore{path: filepath.Join(dir, secretsFileName)}
}

// load returns the stored keys. A missing or empty file is an empty store: the
// bridge starts without credentials and the panel reports "key not set" instead
// of refusing to start.
func (s *secretStore) load() (providerSecrets, error) {
	empty := providerSecrets{Providers: map[string]string{}}
	if s == nil {
		return empty, nil
	}
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, errors.New("cannot read the secrets file")
	}
	s.warn.Do(func() {
		if info, statErr := os.Stat(s.path); statErr == nil && info.Mode().Perm()&0o077 != 0 {
			slog.Warn("secrets_permissions_wide", "path", s.path, "mode", info.Mode().Perm().String())
		}
	})
	if len(raw) == 0 {
		return empty, nil
	}
	// The file is ours and strict: a typo in a provider name is reported
	// instead of silently dropping the key of the role that needed it.
	var store providerSecrets
	if err := json.Unmarshal(raw, &store, json.RejectUnknownMembers(true)); err != nil {
		return empty, errors.New("invalid secrets JSON")
	}
	if store.Providers == nil {
		store.Providers = map[string]string{}
	}
	return store, nil
}

// update replaces the whole key table. The previous file is kept next to it
// under the settings snapshot suffix, so the configuration write can roll both
// files back together.
func (s *secretStore) update(store providerSecrets) error {
	if s == nil {
		return errors.New("no secrets store")
	}
	if len(store.Providers) == 0 {
		store.Providers = map[string]string{}
	}
	data, err := json.Marshal(store, jsontext.WithIndent("  "))
	if err != nil {
		return errors.New("cannot encode the secrets file")
	}
	previous, err := os.ReadFile(s.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot read the secrets file")
	}
	// An empty snapshot means there was no file before, so a rollback removes it.
	if err := writeFileAtomic(s.path+settingsSnapshotSuffix, previous); err != nil {
		return errors.New("cannot keep the previous secrets")
	}
	if err := writeFileAtomic(s.path, data); err != nil {
		return errors.New("cannot write the secrets file")
	}
	return nil
}

// restore returns the secrets file to the snapshot taken by update. A
// zero-length snapshot means the file did not exist, so it is removed again.
func (s *secretStore) restore() error {
	if s == nil {
		return nil
	}
	backup, err := os.ReadFile(s.path + settingsSnapshotSuffix)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(backup) == 0 {
		if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return writeFileAtomic(s.path, backup)
}

// validateAPIKey rejects a value that cannot be sent as a bearer token and caps
// its length. The value is never echoed back in the error.
func validateAPIKey(key string) error {
	if key == "" {
		return nil
	}
	if len(key) > maxAPIKeyBytes {
		return fmt.Errorf("api key is longer than %d bytes", maxAPIKeyBytes)
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return errors.New("api key contains control characters")
		}
	}
	return nil
}
