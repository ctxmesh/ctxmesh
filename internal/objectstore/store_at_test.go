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
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

// A missing key is told apart from an unreachable store, on the S3 store and the in-memory one, so a
// caller can back off from a store without treating every missing object as an outage.
func TestIsNotFound(t *testing.T) {
	_, err := NewMemObjectStore().Get(context.Background(), "skills/team-a/missing")
	if !IsNotFound(err) {
		t.Fatalf("a missing key in the memory store is not found: %v", err)
	}
	s3 := fmt.Errorf("get KB object %q: %w", "k", minio.ErrorResponse{Code: "NoSuchKey", StatusCode: http.StatusNotFound})
	if !IsNotFound(s3) {
		t.Fatal("NoSuchKey is not found")
	}
	if IsNotFound(errors.New("dial tcp: connection refused")) {
		t.Fatal("an unreachable store is not a missing key")
	}
	if IsNotFound(fmt.Errorf("x: %w", minio.ErrorResponse{Code: "AccessDenied", StatusCode: http.StatusForbidden})) {
		t.Fatal("a refused request is not a missing key")
	}
}

func TestNewStoreAt(t *testing.T) {
	if _, err := NewStoreAt("objects.example:9000", "a", "b", time.Second); err != nil {
		t.Fatalf("a well-formed address builds a store: %v", err)
	}
	if _, err := NewStoreAt("bad addr with spaces:x", "a", "b", 0); err == nil {
		t.Fatal("a malformed address is refused")
	}
}
