// Package fakeclient provides an importable fake runtime.Client for tests
// that live outside internal/docker (where the canonical in-memory fake
// currently lives inside a _test.go file and therefore cannot be imported).
//
// Client embeds runtime.Client and overrides only the methods a given test
// needs. Any method NOT overridden resolves to the embedded (nil) interface
// and PANICS on call — deliberately: a nil-interface method call surfaces a
// test that exercises an unexpected code path instead of silently returning
// a zero value.
package fakeclient

import (
	"context"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// Client is a fake runtime.Client. Embed the zero value and override the
// methods your test uses; any other method call panics.
type Client struct {
	runtime.Client // unimplemented methods panic (nil interface call)

	// InfoResult / InfoErr are returned by Info.
	InfoResult runtime.Info
	InfoErr    error

	// NodeListResult / NodeListErr are returned by NodeList.
	NodeListResult []runtime.Node
	NodeListErr    error
}

// Info implements runtime.Client.
func (c *Client) Info(_ context.Context) (runtime.Info, error) {
	return c.InfoResult, c.InfoErr
}

// NodeList implements runtime.Client.
func (c *Client) NodeList(_ context.Context) ([]runtime.Node, error) {
	return c.NodeListResult, c.NodeListErr
}

// Compile-time assertion: Client satisfies the runtime.Client interface via
// the embedded nil interface + the overridden methods.
var _ runtime.Client = (*Client)(nil)
