package qmd

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/marcus/recall/pkg/protocol"
)

// HTTPInference is an explicit opt-in. There is no discovery, fallback, or
// dependency on a particular inference service in Recall's application core.
type HTTPInference struct {
	Collection   string `json:"collection"`
	URL          string `json:"url"`
	TokenizerURL string `json:"tokenizer_url"`
	Model        string `json:"model"`
	Dimensions   int    `json:"dimensions"`
	Package      string `json:"package"`
	Index        string `json:"index"`
	Node         string `json:"-"`
	APIKeyEnv    string `json:"api_key_env,omitempty"`
	Rook         bool   `json:"rook,omitempty"`
}

func parseInference(raw map[string]any, mode Mode, replay string) (*HTTPInference, error) {
	v, ok := raw["inference_provider"]
	if !ok {
		for key := range raw {
			if strings.HasPrefix(key, "inference_") {
				return nil, protocol.Errorf(protocol.CodeInvalidParams, "qmd: %s requires inference_provider", key)
			}
		}
		return nil, nil
	}
	if v != "openai" {
		return nil, protocol.Errorf(protocol.CodeInvalidParams, "qmd: inference_provider must be openai")
	}
	if mode != ModeVector || replay != "" {
		return nil, protocol.Errorf(protocol.CodeInvalidParams, "qmd: HTTP inference supports vector mode only, without replay; hybrid/full expansion and reranking are unsupported")
	}
	p := &HTTPInference{}
	p.Collection, _ = raw["collection"].(string)
	p.Collection = strings.TrimSpace(p.Collection)
	var err error
	for key, dst := range map[string]*string{"inference_url": &p.URL, "inference_tokenizer_url": &p.TokenizerURL, "inference_model": &p.Model, "inference_qmd_package": &p.Package, "inference_index": &p.Index, "inference_api_key_env": &p.APIKeyEnv, "inference_node": &p.Node} {
		if *dst, err = stringSetting(raw, key, ""); err != nil {
			return nil, err
		}
	}
	u, err := url.Parse(p.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, protocol.Errorf(protocol.CodeInvalidParams, "qmd: inference_url must be an HTTP(S) service root without credentials, query, or fragment")
	}
	p.URL = strings.TrimRight(p.URL, "/")
	if p.TokenizerURL == "" {
		p.TokenizerURL = p.URL
	}
	t, err := url.Parse(p.TokenizerURL)
	if err != nil || (t.Scheme != "http" && t.Scheme != "https") || t.Host == "" || t.User != nil || t.RawQuery != "" || t.Fragment != "" {
		return nil, protocol.Errorf(protocol.CodeInvalidParams, "qmd: inference_tokenizer_url must be an HTTP(S) service root without credentials, query, or fragment")
	}
	p.TokenizerURL = strings.TrimRight(p.TokenizerURL, "/")
	if strings.TrimSpace(p.Model) == "" || strings.ContainsAny(p.Model, "\r\n") {
		return nil, protocol.Errorf(protocol.CodeInvalidParams, "qmd: inference_model must name an immutable embedding identity")
	}
	if p.Dimensions, err = intSetting(raw, "inference_dimensions", 0, 1); err != nil {
		return nil, err
	}
	if p.Dimensions < 1 {
		return nil, protocol.Errorf(protocol.CodeInvalidParams, "qmd: inference_dimensions is required and must be positive")
	}
	if p.Dimensions > 65536 {
		return nil, protocol.Errorf(protocol.CodeInvalidParams, "qmd: inference_dimensions must be at most 65536")
	}
	for key, value := range map[string]string{"inference_qmd_package": p.Package, "inference_index": p.Index} {
		if !filepath.IsAbs(value) {
			return nil, protocol.Errorf(protocol.CodeInvalidParams, "qmd: %s must be an absolute path", key)
		}
	}
	if p.APIKeyEnv != "" && !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(p.APIKeyEnv) {
		return nil, protocol.Errorf(protocol.CodeInvalidParams, "qmd: inference_api_key_env must be an environment variable name")
	}
	if p.Node == "" {
		p.Node = "node"
	}
	if v, ok := raw["inference_rook"]; ok {
		var valid bool
		p.Rook, valid = v.(bool)
		if !valid {
			return nil, protocol.Errorf(protocol.CodeInvalidParams, "qmd: inference_rook must be boolean")
		}
	}
	return p, nil
}

func (p HTTPInference) identity() string {
	// URLs and authentication are routing, not vector-space identity.
	data, _ := json.Marshal([]any{p.Model, p.Dimensions, "qmd-gemma-v1", "provider-tokenizer-v1"})
	return fmt.Sprintf("http-embedding-%x", sha256.Sum256(data))
}

//go:embed provider.mjs
var providerBridge string

type inferenceRunner struct {
	ExecRunner
	provider HTTPInference
}

func (r inferenceRunner) Run(ctx context.Context, args ...string) (Result, error) {
	data, err := json.Marshal(r.provider)
	if err != nil {
		return Result{}, err
	}
	child := r.ExecRunner
	child.Binary = r.provider.Node
	child.Env = append(os.Environ(), "RECALL_QMD_INFERENCE="+string(data))
	return child.Run(ctx, append([]string{"--input-type=module", "--eval", providerBridge, "--"}, args...)...)
}

// CheckInferencePackage verifies the upstream seams and formatter without a
// corpus, index, network request, or local model load.
func CheckInferencePackage(ctx context.Context, packagePath, node string) (Result, error) {
	if node == "" {
		node = "node"
	}
	r := inferenceRunner{ExecRunner: ExecRunner{}, provider: HTTPInference{Package: packagePath, Node: node}}
	return r.Run(ctx, "check-compatibility")
}
