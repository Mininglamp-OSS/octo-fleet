package runtime

import (
	"encoding/json"
	"strings"
	"testing"
)

// The cc-octo install secret may carry an optional model id; it must round-trip
// through the relay (ccOctoSecret) and the fetch response, and be omitted from
// the JSON when empty so a model-less install stays backward compatible.
func TestCcOctoConfigResponse_SerializesModel(t *testing.T) {
	b, err := json.Marshal(ccOctoConfigResponse{GatewayURL: "https://gw", APIKey: "sk", Model: "vertexai/claude-opus-4-8"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"model":"vertexai/claude-opus-4-8"`) {
		t.Fatalf("model not serialized: %s", b)
	}

	b2, err := json.Marshal(ccOctoConfigResponse{GatewayURL: "https://gw", APIKey: "sk"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b2), `"model"`) {
		t.Fatalf("empty model must be omitted: %s", b2)
	}
}

func TestCcOctoSecret_CarriesModel(t *testing.T) {
	s := ccOctoSecret{GatewayURL: "https://gw", APIKey: "sk", Model: "m1"}
	if s.Model != "m1" {
		t.Fatalf("got %q", s.Model)
	}
}
