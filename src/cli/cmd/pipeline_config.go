package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// loadPipelineConfig reads a pipeline definition file (-f), accepting YAML or JSON.
//
// It returns the raw state-machine document when the file IS one — recognised by a
// top-level `states:` key — leaving conversion to the workflows service (which owns
// the single state-machine ⇄ steps/routes/maps converter). Otherwise it returns the
// parsed legacy pipelineFile (the flat steps/routes shape). Exactly one of sm / pf is
// meaningful, told apart by isSM.
func loadPipelineConfig(path string) (isSM bool, sm json.RawMessage, pf pipelineFile, err error) {
	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		return false, nil, pf, fmt.Errorf("reading pipeline file: %w", rerr)
	}
	// YAML is a JSON superset, so this converts YAML and passes plain JSON through
	// unchanged — one path handles both extensions.
	jsonBytes, yerr := yaml.YAMLToJSON(raw)
	if yerr != nil {
		return false, nil, pf, fmt.Errorf("parsing pipeline file: %w", yerr)
	}
	var probe map[string]json.RawMessage
	if uerr := json.Unmarshal(jsonBytes, &probe); uerr != nil {
		return false, nil, pf, fmt.Errorf("parsing pipeline file: %w", uerr)
	}
	if _, ok := probe["states"]; ok {
		return true, json.RawMessage(jsonBytes), pf, nil
	}
	if uerr := json.Unmarshal(jsonBytes, &pf); uerr != nil {
		return false, nil, pf, fmt.Errorf("parsing pipeline file: %w", uerr)
	}
	return false, nil, pf, nil
}

// stateMachineName peeks a state-machine document's declared name, so the CLI's
// repo/branch default only applies when the document itself names nothing.
func stateMachineName(sm json.RawMessage) string {
	var meta struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(sm, &meta)
	return meta.Name
}
