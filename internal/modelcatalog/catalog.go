// Package modelcatalog loads public model aliases and resolves provider routes.
package modelcatalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/dio/rute/internal/accountstore"
)

const maxCatalogBytes = 1 << 20

// Model is one public-to-upstream model mapping.
type Model struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	UpstreamModel string `json:"upstream_model"`
	DisplayName   string `json:"display_name,omitempty"`
}

// Resolution is the selected provider and upstream model.
type Resolution struct {
	PublicModel   string
	Provider      string
	UpstreamModel string
	DisplayName   string
	Mapped        bool
}

// Catalog contains validated model mappings.
type Catalog struct {
	models   []Model
	byID     map[string]Model
	filePath string
}

// Empty returns a catalog with fallback namespace routing only.
func Empty() *Catalog {
	return &Catalog{byID: make(map[string]Model)}
}

// Load reads and validates a strict JSON model mapping array.
func Load(path string) (*Catalog, error) {
	if strings.TrimSpace(path) == "" {
		return Empty(), nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("modelcatalog: resolve path: %w", err)
	}
	file, err := os.Open(absolute)
	if err != nil {
		return nil, fmt.Errorf("modelcatalog: open %s: %w", absolute, err)
	}
	defer func() {
		_ = file.Close()
	}()
	decoder := json.NewDecoder(io.LimitReader(file, maxCatalogBytes+1))
	decoder.DisallowUnknownFields()
	var models []Model
	if err := decoder.Decode(&models); err != nil {
		return nil, fmt.Errorf("modelcatalog: decode %s: %w", absolute, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("modelcatalog: file contains trailing data")
	}
	if info, err := file.Stat(); err != nil {
		return nil, fmt.Errorf("modelcatalog: stat %s: %w", absolute, err)
	} else if info.Size() > maxCatalogBytes {
		return nil, errors.New("modelcatalog: file exceeds 1 MiB")
	}

	catalog := &Catalog{
		models:   make([]Model, 0, len(models)),
		byID:     make(map[string]Model, len(models)),
		filePath: absolute,
	}
	for index, model := range models {
		normalized, err := normalizeModel(model)
		if err != nil {
			return nil, fmt.Errorf("modelcatalog: model %d: %w", index, err)
		}
		if _, exists := catalog.byID[normalized.ID]; exists {
			return nil, fmt.Errorf("modelcatalog: duplicate model ID %q", normalized.ID)
		}
		catalog.models = append(catalog.models, normalized)
		catalog.byID[normalized.ID] = normalized
	}
	return catalog, nil
}

// Path returns the absolute source path, or an empty string for an empty catalog.
func (catalog *Catalog) Path() string {
	if catalog == nil {
		return ""
	}
	return catalog.filePath
}

// Resolve applies explicit mappings, namespaces, then the Codex default.
func (catalog *Catalog) Resolve(model, defaultModel string) (Resolution, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = strings.TrimSpace(defaultModel)
	}
	if model == "" {
		return Resolution{}, errors.New("model is required")
	}
	if catalog != nil {
		if mapped, ok := catalog.byID[model]; ok {
			return Resolution{
				PublicModel:   mapped.ID,
				Provider:      mapped.Provider,
				UpstreamModel: mapped.UpstreamModel,
				DisplayName:   mapped.DisplayName,
				Mapped:        true,
			}, nil
		}
	}
	for _, provider := range []string{
		accountstore.ProviderClaude,
		accountstore.ProviderCodex,
	} {
		prefix := provider + "/"
		if strings.HasPrefix(model, prefix) {
			upstream := strings.TrimSpace(strings.TrimPrefix(model, prefix))
			if upstream == "" {
				return Resolution{}, fmt.Errorf("model namespace %q has an empty model", provider)
			}
			return Resolution{
				PublicModel:   model,
				Provider:      provider,
				UpstreamModel: upstream,
			}, nil
		}
	}
	return Resolution{
		PublicModel:   model,
		Provider:      accountstore.ProviderCodex,
		UpstreamModel: model,
	}, nil
}

// Models returns configured mappings plus the default Codex pass-through IDs.
func (catalog *Catalog) Models(defaultModel string) []Model {
	byID := make(map[string]Model)
	if catalog != nil {
		for _, model := range catalog.models {
			byID[model.ID] = model
		}
	}
	defaultModel = strings.TrimSpace(defaultModel)
	if defaultModel != "" {
		byID[defaultModel] = Model{
			ID:            defaultModel,
			Provider:      accountstore.ProviderCodex,
			UpstreamModel: defaultModel,
		}
		namespaced := accountstore.ProviderCodex + "/" + defaultModel
		byID[namespaced] = Model{
			ID:            namespaced,
			Provider:      accountstore.ProviderCodex,
			UpstreamModel: defaultModel,
		}
	}
	models := make([]Model, 0, len(byID))
	for _, model := range byID {
		models = append(models, model)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models
}

func normalizeModel(model Model) (Model, error) {
	model.ID = strings.TrimSpace(model.ID)
	model.Provider = strings.TrimSpace(model.Provider)
	model.UpstreamModel = strings.TrimSpace(model.UpstreamModel)
	model.DisplayName = strings.TrimSpace(model.DisplayName)
	if err := validateValue("id", model.ID, 128); err != nil {
		return Model{}, err
	}
	if strings.IndexFunc(model.ID, unicode.IsSpace) >= 0 {
		return Model{}, errors.New("id must not contain whitespace")
	}
	if model.Provider != accountstore.ProviderCodex &&
		model.Provider != accountstore.ProviderClaude {
		return Model{}, fmt.Errorf("unsupported provider %q", model.Provider)
	}
	if err := validateValue("upstream_model", model.UpstreamModel, 256); err != nil {
		return Model{}, err
	}
	if strings.IndexFunc(model.UpstreamModel, unicode.IsSpace) >= 0 {
		return Model{}, errors.New("upstream_model must not contain whitespace")
	}
	for _, prefix := range []string{
		accountstore.ProviderCodex + "/",
		accountstore.ProviderClaude + "/",
	} {
		if strings.HasPrefix(model.UpstreamModel, prefix) {
			return Model{}, errors.New("upstream_model must not contain a provider namespace")
		}
	}
	if model.DisplayName != "" {
		if err := validateValue("display_name", model.DisplayName, 256); err != nil {
			return Model{}, err
		}
	}
	return model, nil
}

func validateValue(name, value string, maximum int) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > maximum {
		return fmt.Errorf("%s exceeds %d bytes", name, maximum)
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("%s contains control characters", name)
	}
	return nil
}
