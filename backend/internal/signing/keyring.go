package signing

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

type ManifestTrustKey struct {
	ID        string `json:"id"`
	PublicKey string `json:"public_key"`
}

type ManifestTrustConfig struct {
	KeysetVersion *int               `json:"keyset_version,omitempty"`
	Keys          []ManifestTrustKey `json:"keys"`
}

type ManifestKeyringEntry struct {
	KeyID             string `json:"key_id,omitempty"`
	PrivateSeedBase64 string `json:"private_seed_base64"`
}

type ManifestKeyringConfig struct {
	DefaultKeyID string                 `json:"default_key_id,omitempty"`
	Keys         []ManifestKeyringEntry `json:"keys"`
}

type ManifestSignerSet struct {
	defaultKeyID string
	signers      map[string]*ManifestSigner
}

func NewManifestSignerSet(signers []*ManifestSigner, defaultKeyID string) (*ManifestSignerSet, error) {
	if len(signers) == 0 {
		return nil, errors.New("manifest signer set requires at least one signer")
	}

	result := &ManifestSignerSet{
		defaultKeyID: strings.TrimSpace(defaultKeyID),
		signers:      make(map[string]*ManifestSigner, len(signers)),
	}
	for _, signer := range signers {
		if signer == nil {
			return nil, errors.New("manifest signer set contains a nil signer")
		}
		if _, exists := result.signers[signer.KeyID()]; exists {
			return nil, fmt.Errorf("duplicate manifest signing key id %q", signer.KeyID())
		}
		result.signers[signer.KeyID()] = signer
	}

	if result.defaultKeyID == "" && len(result.signers) == 1 {
		for keyID := range result.signers {
			result.defaultKeyID = keyID
		}
	}
	if result.defaultKeyID != "" {
		if _, exists := result.signers[result.defaultKeyID]; !exists {
			return nil, fmt.Errorf("unknown default manifest signing key id %q", result.defaultKeyID)
		}
	}

	return result, nil
}

func NewSingleManifestSignerSet(signer *ManifestSigner) *ManifestSignerSet {
	set, err := NewManifestSignerSet([]*ManifestSigner{signer}, signer.KeyID())
	if err != nil {
		panic(err)
	}
	return set
}

func NewManifestSignerSetFromConfig(config ManifestKeyringConfig) (*ManifestSignerSet, error) {
	if len(config.Keys) == 0 {
		return nil, errors.New("manifest keyring must contain at least one key")
	}

	signers := make([]*ManifestSigner, 0, len(config.Keys))
	for _, entry := range config.Keys {
		signer, err := NewManifestSignerFromSeedBase64(entry.PrivateSeedBase64, entry.KeyID)
		if err != nil {
			return nil, fmt.Errorf("build signer for key %q: %w", entry.KeyID, err)
		}
		signers = append(signers, signer)
	}

	return NewManifestSignerSet(signers, config.DefaultKeyID)
}

func LoadManifestSignerSetFromFile(path string) (*ManifestSignerSet, error) {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest keyring file: %w", err)
	}

	var config ManifestKeyringConfig
	if err := json.Unmarshal(bytes, &config); err != nil {
		return nil, fmt.Errorf("decode manifest keyring file: %w", err)
	}
	return NewManifestSignerSetFromConfig(config)
}

func (s *ManifestSignerSet) DefaultKeyID() string {
	if s == nil {
		return ""
	}
	return s.defaultKeyID
}

func (s *ManifestSignerSet) ResolveKeyID(requestedKeyID string) (string, error) {
	requestedKeyID = strings.TrimSpace(requestedKeyID)
	if s == nil {
		if requestedKeyID != "" {
			return "", fmt.Errorf("manifest signing key %q requested but no signer set is configured", requestedKeyID)
		}
		return "", nil
	}

	if requestedKeyID != "" {
		if _, exists := s.signers[requestedKeyID]; !exists {
			return "", fmt.Errorf("unknown manifest signing key id %q", requestedKeyID)
		}
		return requestedKeyID, nil
	}

	if s.defaultKeyID != "" {
		return s.defaultKeyID, nil
	}
	if len(s.signers) == 1 {
		for keyID := range s.signers {
			return keyID, nil
		}
	}
	return "", errors.New("manifest signing key id is required because multiple signer keys are configured")
}

func (s *ManifestSignerSet) SignerForKeyID(keyID string) (*ManifestSigner, error) {
	keyID, err := s.ResolveKeyID(keyID)
	if err != nil {
		return nil, err
	}
	if keyID == "" {
		return nil, nil
	}
	return s.signers[keyID], nil
}

func (s *ManifestSignerSet) ManifestTrustConfig(keysetVersion *int) ManifestTrustConfig {
	keys := make([]ManifestTrustKey, 0, len(s.signers))
	for _, signer := range s.signers {
		keys = append(keys, ManifestTrustKey{
			ID:        signer.KeyID(),
			PublicKey: signer.PublicKeyBase64(),
		})
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ID != keys[j].ID {
			return keys[i].ID < keys[j].ID
		}
		return keys[i].PublicKey < keys[j].PublicKey
	})
	return ManifestTrustConfig{
		KeysetVersion: keysetVersion,
		Keys:          keys,
	}
}
