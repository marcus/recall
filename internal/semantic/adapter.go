package semantic

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/marcus/recall/internal/adapters/docs"
	"github.com/marcus/recall/internal/semantic/antfly"
	"github.com/marcus/recall/internal/semantic/input"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/document"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// Change this when persisted chunking, embedding input, or index semantics change.
const indexFormat = 1

type embedder interface {
	Embed(context.Context, []string, bool) ([][]float32, error)
	EmbedDocuments(context.Context, []string, []string) ([][]float32, error)
}

type searchIndex interface {
	Search(context.Context, []float32, int) ([]antfly.Hit, error)
	Close() error
}

type Adapter struct {
	root, dir, source, config, model string
	dimensions                       int
	floor                            float64
	documentSettings                 map[string]any

	embed embedder
	build func(context.Context, string, int, []antfly.Row) error
	open  func(string) (searchIndex, error)
}

func New() *Adapter {
	return &Adapter{
		build: antfly.Build,
		open: func(file string) (searchIndex, error) {
			return antfly.Open(file)
		},
	}
}

func (a *Adapter) Initialize(_ context.Context, cfg adapter.Config) (recall.Manifest, error) {
	version, err := protocol.NegotiateVersion(cfg.ProtocolVersionMin, cfg.ProtocolVersionMax)
	if err != nil {
		return recall.Manifest{}, err
	}
	if !filepath.IsAbs(cfg.Location) {
		return recall.Manifest{}, fmt.Errorf("recall-antfly requires an absolute corpus location")
	}
	root, err := filepath.EvalSymlinks(cfg.Location)
	if err != nil {
		return recall.Manifest{}, err
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return recall.Manifest{}, fmt.Errorf("recall-antfly corpus is unavailable")
	}
	workdir := cfg.SharedWorkdir
	if workdir == "" {
		workdir = cfg.Workdir
	}
	if !filepath.IsAbs(workdir) {
		return recall.Manifest{}, fmt.Errorf("recall-antfly requires an absolute state directory")
	}

	docSettings := map[string]any{}
	var url, model, keyEnv string
	dimensions, floor := 768, 0.30
	for name, value := range cfg.Settings {
		switch name {
		case "extensions", "exclude_dirs", "exclude_globs", "exclude_nested_repos", "sibling_preference", "max_raw_bytes", "max_file_bytes":
			docSettings[name] = value
		case "inference_url", "inference_model", "inference_api_key_env":
			str, ok := value.(string)
			if !ok || str == "" {
				return recall.Manifest{}, fmt.Errorf("settings.%s must be a nonempty string", name)
			}
			switch name {
			case "inference_url":
				url = str
			case "inference_model":
				model = str
			case "inference_api_key_env":
				keyEnv = str
			}
		case "inference_dimensions", "cosine_floor":
			var n float64
			switch value := value.(type) {
			case float64:
				n = value
			case int64:
				n = float64(value)
			case int:
				n = float64(value)
			case json.Number:
				n, err = value.Float64()
			default:
				err = fmt.Errorf("invalid number")
			}
			if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
				return recall.Manifest{}, fmt.Errorf("settings.%s must be a finite number", name)
			}
			if name == "cosine_floor" {
				if n < 0 || n > 1 {
					return recall.Manifest{}, fmt.Errorf("settings.cosine_floor must be between 0 and 1")
				}
				floor = n
			} else {
				if n < 1 || n > 4096 || n != math.Trunc(n) {
					return recall.Manifest{}, fmt.Errorf("settings.inference_dimensions must be an integer from 1 to 4096")
				}
				dimensions = int(n)
			}
		default:
			return recall.Manifest{}, fmt.Errorf("unknown recall-antfly setting %q", name)
		}
	}
	client, err := input.New(url, model, keyEnv, dimensions)
	if err != nil {
		return recall.Manifest{}, err
	}
	fingerprint, err := json.Marshal([]any{indexFormat, document.HTMLExtractionVersion, root, docSettings, model, dimensions})
	if err != nil {
		return recall.Manifest{}, err
	}
	a.root, a.source, a.model = root, cfg.SourceID, model
	a.dir = filepath.Join(workdir, "antfly")
	a.config = fmt.Sprintf("%x", sha256.Sum256(fingerprint))
	a.documentSettings, a.floor, a.dimensions, a.embed = docSettings, floor, dimensions, client
	return recall.Manifest{
		ProtocolVersion: version,
		AdapterID:       "recall-antfly.v1",
		DisplayName:     "Antfly Lite documents",
		RecordTypes:     []recall.RecordType{recall.RecordDocument},
		QueryModes:      []recall.QueryMode{recall.QuerySemantic},
		FreshnessModes:  []recall.FreshnessMode{recall.FreshnessIndexed},
		AsOfSupport:     recall.AsOfNone,
		RelevanceBasis:  recall.RelevanceVectorSimilarity,
		Capabilities:    []recall.Capability{recall.CapSearch, recall.CapExpand, recall.CapCheckpoint},
		FreshnessPolicy: "refresh verifies and atomically publishes a generation; changed and deleted files are withheld until publication",
		Sensitivity:     recall.SensitivityInternal,
	}, nil
}

func (a *Adapter) scan(ctx context.Context) (docs.SemanticScan, error) {
	return docs.ScanSemantic(ctx, a.root, a.documentSettings)
}

func (a *Adapter) read(path string) (docs.SemanticDocument, error) {
	return docs.ReadSemantic(a.root, path, a.documentSettings)
}

func (a *Adapter) Close() error { return nil }

func withDeadline(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	if deadline.IsZero() {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, deadline)
}

var _ adapter.Adapter = (*Adapter)(nil)
