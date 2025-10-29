package appws

import "errors"

// ErrNotImplemented is returned by scaffold stubs.
var ErrNotImplemented = errors.New("not implemented")

// ErrClientClosed indicates the client connection was closed or not yet dialed.
var ErrClientClosed = errors.New("appws: client closed")
