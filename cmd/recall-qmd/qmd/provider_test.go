package qmd

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func inferenceSettings() map[string]any {
	return map[string]any{"collection": "notes", "mode": "vector", "inference_provider": "openai", "inference_url": "http://localhost:1234", "inference_model": "embeddinggemma-300m-q8-0-sha256", "inference_dimensions": 768, "inference_qmd_package": "/opt/qmd", "inference_index": "/tmp/dedicated-provider.sqlite"}
}

func TestInferenceRequiresExplicitCompleteOptIn(t *testing.T) {
	local, err := parseSettings(map[string]any{"collection": "notes", "mode": "vector"}, "")
	if err != nil || local.Inference != nil {
		t.Fatalf("local configuration changed: %+v %v", local, err)
	}
	cases := []struct {
		key    string
		value  any
		remove bool
	}{
		{"inference_provider", "rook", false}, {"inference_provider", nil, true},
		{"inference_dimensions", 0, false}, {"inference_dimensions", 1.5, false},
		{"inference_model", "", false}, {"inference_index", "relative.sqlite", false},
		{"inference_qmd_package", "relative", false},
		{"inference_url", "http://secret:password@host", false},
		{"inference_url", "http://host?key=secret", false},
		{"inference_tokenizer_url", "file:///tmp/tokenizer", false},
		{"inference_api_key_env", "token value", false},
		{"mode", "hybrid", false}, {"mode", "full", false}, {"mode", "bm25", false},
		{"replay", "/tmp/fixture", false}, {"inference_rook", "true", false},
	}
	for _, tc := range cases {
		t.Run(tc.key+"/"+strings.ReplaceAll(strings.TrimSpace(toString(tc.value)), "/", "_"), func(t *testing.T) {
			raw := inferenceSettings()
			if tc.remove {
				delete(raw, tc.key)
			} else {
				raw[tc.key] = tc.value
			}
			if _, err := parseSettings(raw, ""); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestInferenceIdentityPinsSpaceButNotRouting(t *testing.T) {
	raw := inferenceSettings()
	set, err := parseSettings(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	original := set.Inference.identity()
	if set.Inference.TokenizerURL != set.Inference.URL {
		t.Fatal("tokenizer URL did not default to provider")
	}
	set.Inference.URL = "https://different-provider.example"
	set.Inference.TokenizerURL = "https://tokenizer.example"
	set.Inference.APIKeyEnv = "OTHER_KEY"
	if got := set.Inference.identity(); got != original {
		t.Fatalf("routing changed embedding space: %s", got)
	}
	set.Inference.Dimensions++
	if set.Inference.identity() == original {
		t.Fatal("dimensions not pinned")
	}
	set.Inference.Dimensions--
	set.Inference.Model += "-different"
	if set.Inference.identity() == original {
		t.Fatal("model not pinned")
	}
	if !strings.Contains(set.settingsDigest(), "expansion=none") {
		t.Fatal("explicit expansion semantics missing")
	}
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return "value"
}

func TestHTTPInferenceContracts(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is optional; required only for configured HTTP inference")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, "--test", "provider_http_test.mjs")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("HTTP inference contracts: %v\n%s", err, output)
	}
}
