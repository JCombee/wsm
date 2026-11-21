package main

import (
	"fmt"
	"time"
)

// runAuth implements "wsm auth enroll": a one-time link that registers a
// passkey for the dashboard (ADR 0016). It is also the way back in when
// every passkey is lost.
func runAuth(e env) error {
	if len(e.args) != 1 || e.args[0] != "enroll" {
		return usageError{"want: auth enroll"}
	}
	link, err := apiClient(e).Enroll(e.ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "open this link within %s to register a passkey:\n%s\n",
		time.Until(link.ExpiresAt).Round(time.Minute), link.URL)
	return nil
}
