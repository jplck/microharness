package cmd

import "testing"

func TestExternalPluginOptionsRemoved(t *testing.T) {
	if serveCmd.Flags().Lookup("plugins") != nil {
		t.Fatal("serve still accepts an external plugin directory")
	}
	for _, command := range rootCmd.Commands() {
		if command.Name() == "plugins" {
			t.Fatal("external plugin management command is still registered")
		}
	}
}
