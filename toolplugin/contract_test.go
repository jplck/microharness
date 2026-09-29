package toolplugin

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestContract(t *testing.T) {
	tools := []Tool{{Definition: Definition{Name: "echo", Description: "Echo input"}, Call: Handler(func(arguments struct{ Input string }) (string, error) { return arguments.Input, nil })}}
	var output bytes.Buffer
	if err := Run([]string{"describe"}, nil, &output, tools); err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(output.Bytes(), &manifest); err != nil || manifest.ProtocolVersion != 1 || len(manifest.Tools) != 1 {
		t.Fatalf("manifest: %s %v", output.String(), err)
	}
	for _, test := range []struct {
		name, input, result string
		failed              bool
	}{
		{"echo", `{"protocol_version":1,"arguments":{"input":"hello"}}`, "hello", false},
		{"echo", `{"protocol_version":2,"arguments":{}}`, "", true},
		{"echo", `{"protocol_version":1,"arguments":null}`, "", true},
		{"missing", `{"protocol_version":1,"arguments":{}}`, "", true},
		{"echo", `{"protocol_version":1,"arguments":{}} garbage`, "", true},
	} {
		output.Reset()
		if err := Run([]string{"call", test.name}, strings.NewReader(test.input), &output, tools); err != nil {
			t.Fatal(err)
		}
		var response Response
		if err := json.Unmarshal(output.Bytes(), &response); err != nil || response.ProtocolVersion != 1 || response.Result != test.result || (response.Error != "") != test.failed {
			t.Fatalf("response: %s %v", output.String(), err)
		}
	}
}
