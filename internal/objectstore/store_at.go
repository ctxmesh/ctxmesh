/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package objectstore

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ErrNotFound is the error MemObjectStore wraps for a missing key. Use IsNotFound to test any store's.
var ErrNotFound = errors.New("object not found")

// IsNotFound reports whether err from Get means the key or its bucket does not exist, as opposed to
// the store being unreachable or refusing the request.
func IsNotFound(err error) bool {
	if errors.Is(err, ErrNotFound) {
		return true
	}
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		return resp.Code == "NoSuchKey" || resp.Code == "NoSuchBucket" || resp.StatusCode == http.StatusNotFound
	}
	return false
}

// NewStoreAt builds the durable ObjectStore for an address and credential the caller already holds,
// rather than reading them from OBJECT_STORE_* env as NewMinioStore does. The controller uses it to
// read uploaded skill bodies from the bundled store, whose address and credential it derives for the
// agent pods it renders. It reads and writes the same bucket as NewMinioStore. A positive dialTimeout
// bounds connecting and the wait for response headers, so an unreachable store fails in seconds.
func NewStoreAt(addr, accessKey, secretKey string, dialTimeout time.Duration) (ObjectStore, error) {
	opts := &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: false,
	}
	if dialTimeout > 0 {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.DialContext = (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext
		transport.ResponseHeaderTimeout = dialTimeout
		opts.Transport = transport
	}
	client, err := minio.New(addr, opts)
	if err != nil {
		return nil, fmt.Errorf("durable object store at %q: %w", addr, err)
	}
	return &minioKBStore{client: client, bucket: knowledgeBaseBucket}, nil
}
