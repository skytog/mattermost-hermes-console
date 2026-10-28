package main

import "github.com/mattermost/mattermost/server/public/plugin"

// main is the plugin entry point required by the Mattermost plugin RPC runtime.
func main() {
	plugin.ClientMain(&Plugin{})
}
