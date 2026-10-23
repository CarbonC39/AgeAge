// Package attachment defines the transport boundary for incoming files.
// Connectors expose metadata and a lazy stream; interpretation belongs to the runtime.
package attachment

import (
	"context"
	"io"
)

// Attachment describes one untrusted incoming file. Size is zero when unknown.
// Open must return a fresh stream and honor ctx. The runtime owns closing it.
// URL is provenance only and must never contain credentials.
type Attachment struct {
	Filename string
	MIMEType string
	Size     int64
	URL      string
	Open     func(context.Context) (io.ReadCloser, error)
	// UnsupportedReason allows a connector to retain metadata for media it
	// cannot expose (for example encrypted Matrix files or audio/video).
	UnsupportedReason string
}
