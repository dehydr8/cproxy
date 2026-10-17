// Command cproxy is a proxy for the Anthropic API that pools multiple
// Claude credentials behind a single endpoint and rotates the credential
// sent upstream, per client, with rate-limit failover.
package main

import "github.com/dehydr8/cproxy/cmd"

func main() {
	cmd.Execute()
}
